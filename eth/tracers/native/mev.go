package native

import (
	"bytes"
	"encoding/json"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"math/big"
	"sync/atomic"
	"time"
)

func init() {
	register("mevTracer", newMevTracer)
}

type mevLog struct {
	Address common.Address `json:"address"`
	Topics  []common.Hash  `json:"topics"`
	Data    []byte         `json:"data"`
}
type mevRes struct {
	StateDiff    stateDiff `json:"state_diff"`
	StructLogs   []mevLog  `json:"structured_logs"`
	RevertReason string    `json:"revert_reason"`
}

// 1、列出这笔交易所影响的地址列表，而无需得到具体的修改位置。
// 2、列出这笔交易所释放的Log
// 3、如果失败了，返回这笔交易失败的原因。
type mevTracer struct {
	env                *vm.EVM
	ctx                *tracers.Context // Holds tracer context data
	revertReason       string           // The revert reason return from the tx, if tx success, empty string return
	stateDiff          stateDiff
	structLogs         []mevLog
	initialState       *state.StateDB
	create             bool
	to                 common.Address
	accountsToRemove   []common.Address
	changedStorageKeys map[common.Address]map[common.Hash]bool
	interrupt          uint32 // Atomic flag to signal execution interruption
	reason             error  // Textual reason for the interruption
}

var revertSelector = crypto.Keccak256([]byte("Error(string)"))[:4]

func newMevTracer(ctx *tracers.Context, _ json.RawMessage) (tracers.Tracer, error) {
	// First callframe contains tx context info
	// and is populated on start and end.
	return &mevTracer{stateDiff: stateDiff{}, ctx: ctx,
		changedStorageKeys: make(map[common.Address]map[common.Hash]bool)}, nil
}

func (t *mevTracer) CapturePreEVM(env *vm.EVM) {
	t.env = env
	if t.initialState == nil {
		t.initialState = t.env.StateDB.(*state.StateDB).Copy()
	}
}

// initAccount stores the account address, in order we fetch the data in GetResult
func (t *mevTracer) initAccount(address common.Address, marker *stateDiffMarker) error {
	if _, ok := t.stateDiff[address]; !ok {
		t.stateDiff[address] = &stateDiffAccount{
			marker:  marker,
			Storage: make(map[common.Hash]map[stateDiffMarker]interface{}),
		}
	} else {
		// update the marker if account already inited
		if marker != nil && *marker != "" {
			t.stateDiff[address].marker = marker
		}
	}
	return nil
}

// initStorageKey stores the storage key in the account, in order we fetch the data in GetResult. It assumes `lookupAccount`
// has been performed on the contract before.
func (t *mevTracer) initStorageKey(addr common.Address, key common.Hash) {
	t.stateDiff[addr].Storage[key] = make(map[stateDiffMarker]interface{})
}

func (t *mevTracer) CaptureTxStart(gasLimit uint64) {

}
func (t *mevTracer) CaptureTxEnd(restGas uint64) {

}

func (t *mevTracer) CaptureStart(env *vm.EVM, from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	t.create = create
	t.to = to

	var marker stateDiffMarker
	if create {
		marker = markerBorn
	}

	t.initAccount(from, nil)
	t.initAccount(to, &marker)
}
func (t *mevTracer) CaptureEnd(output []byte, gasUsed uint64, _ time.Duration, err error) {
	if err != nil {
		if err == vm.ErrExecutionReverted && len(output) > 4 && bytes.Equal(output[:4], revertSelector) {
			errMsg, _ := abi.UnpackRevert(output)
			t.revertReason = err.Error() + ": " + errMsg
		} else {
			if err.Error() != "" {
				t.revertReason = err.Error() // may empty
			} else {
				t.revertReason = "revert"
			}
		}
	}
}

func (t *mevTracer) CaptureEnter(typ vm.OpCode, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	// Skip if tracing was interrupted
	if atomic.LoadUint32(&t.interrupt) > 0 {
		t.env.Cancel()
		return
	}
}

func (t *mevTracer) CaptureExit(output []byte, gasUsed uint64, err error) {}

func (t *mevTracer) CaptureState(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, rData []byte, depth int, err error) {
	stack := scope.Stack
	stackData := stack.Data()
	stackLen := len(stackData)
	memory := scope.Memory.Data()

	switch {
	case stackLen >= 1 && (op == vm.SLOAD || op == vm.SSTORE):
		addr := scope.Contract.Address()
		slot := common.Hash(stackData[stackLen-1].Bytes32())
		t.initStorageKey(addr, slot)

		// check if storage set/changed at least once
		if op == vm.SSTORE {
			if _, ok := t.changedStorageKeys[addr]; !ok {
				t.changedStorageKeys[addr] = make(map[common.Hash]bool)
			}

			isValueChanged, found := t.changedStorageKeys[addr][slot]
			if !found {
				t.changedStorageKeys[addr][slot] = false
			}

			if !isValueChanged {
				val := common.Hash(stackData[stackLen-2].Bytes32())
				if val != (common.Hash{}) {
					t.changedStorageKeys[addr][slot] = true
				}
			}
		}
	case stackLen >= 1 && (op == vm.EXTCODECOPY || op == vm.EXTCODEHASH || op == vm.EXTCODESIZE || op == vm.BALANCE):
		addr := common.Address(stackData[stackLen-1].Bytes20())
		t.initAccount(addr, nil)
	case stackLen >= 5 && (op == vm.DELEGATECALL || op == vm.CALL || op == vm.STATICCALL || op == vm.CALLCODE):
		addr := common.Address(stackData[stackLen-2].Bytes20())
		t.initAccount(addr, nil)
	case op == vm.CREATE:
		addr := scope.Contract.Address()
		nonce := t.env.StateDB.GetNonce(addr)
		marker := markerBorn
		t.initAccount(crypto.CreateAddress(addr, nonce), &marker)
	case stackLen >= 4 && op == vm.CREATE2:
		offset := stackData[stackLen-2]
		size := stackData[stackLen-3]
		init := scope.Memory.GetCopy(int64(offset.Uint64()), int64(size.Uint64()))
		inithash := crypto.Keccak256(init)
		salt := stackData[stackLen-4]
		marker := markerBorn
		t.initAccount(crypto.CreateAddress2(scope.Contract.Address(), salt.Bytes32(), inithash), &marker)
	case stackLen >= 1 && op == vm.SELFDESTRUCT:
		addr := common.Address(stackData[stackLen-1].Bytes20())
		t.initAccount(addr, nil)

		// on SELFDESTRUCT mark the contract address as died
		marker := markerDied

		// account won't be SELFDESTRUCTed if out of gas happens on same instruction
		if err != nil && err.Error() == "out of gas" {
			marker = ""
		}
		t.initAccount(scope.Contract.Address(), &marker)
	case stackLen >= 3 && op&0xf0 == vm.LOG0: // log operation
		var topics []common.Hash
		topicCount := int((op & 0xff) - vm.LOG0)
		addr := scope.Contract.Address()
		dataOffset, dataSize := stackData[stackLen-1].Uint64(), stackData[stackLen-2].Uint64()
		var data []byte
		if dataOffset+dataSize <= uint64(len(memory)) {
			data = make([]byte, dataSize)
			copy(data, memory[dataOffset:dataOffset+dataSize])
		}
		for i := 1; i <= topicCount; i++ {
			topics = append(topics, stackData[stackLen-2-i].Bytes32())
		}
		t.structLogs = append(t.structLogs, mevLog{
			Address: addr,
			Topics:  topics,
			Data:    data,
		})
	}

	// log any account errors, in order we decide removal of accounts later
	if err != nil {
		if account, ok := t.stateDiff[scope.Contract.Address()]; ok {
			account.err = err
		}
	}
}
func (t *mevTracer) CaptureFault(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, depth int, err error) {

}

func (t *mevTracer) GetResult() (json.RawMessage, error) {
	t.initAccount(t.env.Context.Coinbase, nil)

	for addr, accountDiff := range t.stateDiff {
		// remove empty accounts
		if t.env.StateDB.Empty(addr) {
			t.accountsToRemove = append(t.accountsToRemove, addr)
			continue
		}

		// read any special predefined marker set
		var marker stateDiffMarker
		if accountDiff.marker != nil {
			marker = *accountDiff.marker
		}

		hasDied := marker == markerDied

		// if an account has been Born within this run and also Died,
		// this means it will never be persisted to the state
		if t.create && addr == t.to && hasDied {
			t.accountsToRemove = append(t.accountsToRemove, addr)
			continue
		}

		// remove accounts with errors, except "out of gas"
		// though, when "out of gas", happens on new account creation, then we remove it as well
		if accountDiff.err != nil &&
			(accountDiff.err.Error() != "out of gas" || marker == markerBorn) {
			t.accountsToRemove = append(t.accountsToRemove, addr)
			continue
		}

		initialExist := t.initialState.Exist(addr)
		exist := t.env.StateDB.Exist(addr)

		// if initialState doesn't have the account (new account creation),
		// and hasDied, then account will be removed from state
		if !initialExist && hasDied {
			t.accountsToRemove = append(t.accountsToRemove, addr)
			continue
		}

		// handle storage keys
		var storageKeysToRemove []common.Hash

		// fill storage
		for key := range accountDiff.Storage {
			hasChanged := false
			if changedKeys, ok := t.changedStorageKeys[addr]; ok {
				if changed, ok := changedKeys[key]; ok && changed {
					hasChanged = true
				}
			}

			fromStorage := t.initialState.GetState(addr, key)
			toStorage := t.env.StateDB.GetState(addr, key)

			if initialExist && exist {
				// mark unchanged storage items for deletion
				if fromStorage == toStorage || (fromStorage == (common.Hash{}) && toStorage == (common.Hash{})) {
					storageKeysToRemove = append(storageKeysToRemove, key)
				} else {
					accountDiff.Storage[key][markerChanged] = &StateDiffStorage{
						From: fromStorage,
						To:   toStorage,
					}
				}
			} else if !initialExist && exist {
				if !hasChanged {
					storageKeysToRemove = append(storageKeysToRemove, key)
					continue
				}
				accountDiff.Storage[key][markerBorn] = toStorage
			} else if initialExist && !exist {
				accountDiff.Storage[key][markerDied] = fromStorage
			}
		}

		// remove marked storage keys
		for _, key := range storageKeysToRemove {
			delete(accountDiff.Storage, key)
		}

		allEqual := len(accountDiff.Storage) == 0

		// account creation
		if !initialExist && exist && !hasDied {
			accountDiff.Nonce = map[stateDiffMarker]hexutil.Uint64{
				markerBorn: hexutil.Uint64(t.env.StateDB.GetNonce(addr)),
			}
			accountDiff.Balance = map[stateDiffMarker]*hexutil.Big{
				markerBorn: (*hexutil.Big)(t.env.StateDB.GetBalance(addr)),
			}
			accountDiff.Code = map[stateDiffMarker]hexutil.Bytes{
				markerBorn: t.env.StateDB.GetCode(addr),
			}

			// account has been removed
		} else if initialExist && !exist || hasDied {
			fromNonce := t.initialState.GetNonce(addr)
			accountDiff.Nonce = map[stateDiffMarker]hexutil.Uint64{
				markerDied: hexutil.Uint64(fromNonce),
			}
			accountDiff.Balance = map[stateDiffMarker]*hexutil.Big{
				markerDied: (*hexutil.Big)(t.initialState.GetBalance(addr)),
			}
			accountDiff.Code = map[stateDiffMarker]hexutil.Bytes{
				markerDied: t.initialState.GetCode(addr),
			}

			// account changed
		} else if initialExist && exist {
			fromNonce := t.initialState.GetNonce(addr)
			toNonce := t.env.StateDB.GetNonce(addr)
			if fromNonce == toNonce {
				accountDiff.Nonce = markerSame
			} else {
				diff := make(map[stateDiffMarker]*StateDiffNonce)
				diff[markerChanged] = &StateDiffNonce{
					From: hexutil.Uint64(fromNonce),
					To:   hexutil.Uint64(toNonce),
				}
				accountDiff.Nonce = diff
				allEqual = false
			}

			fromBalance := t.initialState.GetBalance(addr)
			toBalance := t.env.StateDB.GetBalance(addr)
			if fromBalance.Cmp(toBalance) == 0 {
				accountDiff.Balance = markerSame
			} else {
				diff := make(map[stateDiffMarker]*StateDiffBalance)
				diff[markerChanged] = &StateDiffBalance{From: (*hexutil.Big)(fromBalance), To: (*hexutil.Big)(toBalance)}
				accountDiff.Balance = diff
				allEqual = false
			}

			fromCode := t.initialState.GetCode(addr)
			toCode := t.env.StateDB.GetCode(addr)
			if bytes.Equal(fromCode, toCode) {
				accountDiff.Code = markerSame
			} else {
				diff := make(map[stateDiffMarker]*StateDiffCode)
				diff[markerChanged] = &StateDiffCode{From: fromCode, To: toCode}
				accountDiff.Code = diff
				allEqual = false
			}

			if allEqual {
				t.accountsToRemove = append(t.accountsToRemove, addr)
			}
		} else {
			t.accountsToRemove = append(t.accountsToRemove, addr)
		}
	}

	// remove marked accounts
	for _, addr := range t.accountsToRemove {
		delete(t.stateDiff, addr)
	}

	mevResObj := mevRes{
		StateDiff:    t.stateDiff,
		StructLogs:   t.structLogs,
		RevertReason: t.revertReason,
	}
	res, err := json.Marshal(mevResObj)

	if err != nil {
		return nil, err
	}

	return json.RawMessage(res), t.reason
}

// Stop terminates execution of the tracer at the first opportune moment.
func (t *mevTracer) Stop(err error) {
	t.reason = err
	atomic.StoreUint32(&t.interrupt, 1)
}
