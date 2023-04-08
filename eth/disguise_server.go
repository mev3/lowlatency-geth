package eth

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/rlp"
	"io"
	"math/big"
	"net"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

const (
	GetGenesisMsg             = "1100"
	GetBlockNumber            = "1200"
	GetForksMsg               = "1300"
	GetAllAbove               = "1400"
	GetHeaderByHash           = "2100"
	GetHeaderByNumber         = "2200"
	GetHeaderByHashAndNumber  = "2300"
	GetAncestor               = "3000"
	GetBlockBodyRlpByHash     = "4000"
	GetReceiptsByHash         = "5000"
	GetContractCodeWithPrefix = "6000"
	GetTrieNode               = "7000"
	GetTotalDifficulty        = "8000"
	GetValidateHeader         = "9000"
)

const (
	CmdSize              = 4
	LengthSize           = 4
	HashSize             = 32
	BlockNumberSize      = 8
	ForkNumberSize       = 8
	SendFailedMsg        = "send message to disguise client failed"
	SetWriteDdlFailedMsg = "set deadline for write failed"
	SetReadDdlFailedMsg  = "set deadline for read failed"
	ReadTimeout          = 30 * time.Second
	WriteTimeout         = 20 * time.Second
)

type DisguiseServer struct {
	forkID      forkid.ID
	forks       []uint64
	genesisHash common.Hash
	headerFn    func() uint64

	chain       Blockchain
	curConCount int32
}

// Blockchain defines all necessary method to build a forkID.
type Blockchain interface {
	// Config retrieves the chain's fork configuration.
	Config() *params.ChainConfig

	// CurrentHeader retrieves the current head header of the canonical chain.
	CurrentHeader() *types.Header

	GetHeaderByHash(hash common.Hash) *types.Header
	GetHeaderByNumber(number uint64) *types.Header
	GetHeader(hash common.Hash, number uint64) *types.Header
	GetAncestor(hash common.Hash, number, ancestor uint64, maxNonCanonical *uint64) (common.Hash, uint64)
	GetBodyRLP(hash common.Hash) rlp.RawValue
	GetReceiptsByHash(hash common.Hash) types.Receipts
	ContractCodeWithPrefix(hash common.Hash) ([]byte, error)
	TrieNode(hash common.Hash) ([]byte, error)
	GetTd(hash common.Hash, number uint64) *big.Int
	Engine() consensus.Engine
	GetHighestVerifiedHeader() *types.Header
}

func StartDisguiseServer(ethConfig *ethconfig.Config, genesisHash common.Hash, chain Blockchain) {
	var (
		con      net.Conn
		err      error
		cert     tls.Certificate
		listener net.Listener
		tcpAddr  *net.TCPAddr
	)
	srv := &DisguiseServer{}
	srv.forks = forkid.GatherForks(chain.Config())
	srv.genesisHash = genesisHash
	srv.headerFn = func() uint64 {
		return chain.CurrentHeader().Number.Uint64()
	}
	srv.forkID = forkid.NewID(chain.Config(), genesisHash, srv.headerFn())
	srv.chain = chain

	// load TLS x509 cert file and key file
	cert, err = tls.LoadX509KeyPair(ethConfig.DisguiseServerX509CertFile, ethConfig.DisguiseServerX509KeyFile)
	if err != nil {
		log.Crit("invalid cert file or key file", "reason", err,
			"cert", ethConfig.DisguiseServerX509CertFile, "key", ethConfig.DisguiseServerX509KeyFile)
	}

	// resolve TCP address
	tcpAddr, err = net.ResolveTCPAddr("tcp", ethConfig.DisguiseServerUrl)
	if err != nil {
		log.Crit("invalid listen tcp address", "reason", err)
	}

	// listen for connection
	listener, err = tls.Listen("tcp", tcpAddr.String(), &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		log.Crit("listen on disguise server failed", "reason", err)
	}
	defer listener.Close()
	for {
		con, err = listener.Accept()
		if err != nil {
			log.Warn("accept connection failed", "reason", err)
			continue
		}
		conCopy := con
		go srv.serveDisguiseClient(conCopy)
	}
}

func (ds *DisguiseServer) serveDisguiseClient(conn net.Conn) {
	var (
		err    error
		cmd    string
		buffer = make([]byte, 0x100)
		rdr    = bufio.NewReader(conn)
		wdr    = bufio.NewWriter(conn)
	)
	atomic.AddInt32(&ds.curConCount, 1)
	log.Info("accept disguise client connection", "connection count", atomic.LoadInt32(&ds.curConCount))

	_, err = io.ReadFull(rdr, buffer[:CmdSize])

	for err == nil {
		cmd = string(buffer[:CmdSize])
		err = conn.SetWriteDeadline(time.Now().Add(WriteTimeout))
		if err != nil {
			log.Warn(SetWriteDdlFailedMsg, "reason", err)
		}
		err = conn.SetReadDeadline(time.Now().Add(ReadTimeout))
		if err != nil {
			log.Warn(SetReadDdlFailedMsg, "reason", err)
		}

		switch cmd {
		case GetAllAbove:
			// ============================================
			// 4 bytes: command of message
			// 4 bytes: length of forks (little endian)
			// 32 bytes: genesis hash
			// 8 bytes: block number (little endian)
			// (many) 8 bytes: fork block number
			copy(buffer, GetAllAbove)
			binary.LittleEndian.PutUint32(buffer[CmdSize:], uint32(len(ds.forks)))
			copy(buffer[CmdSize+LengthSize:], ds.genesisHash[:])
			binary.LittleEndian.PutUint64(buffer[CmdSize+LengthSize+HashSize:], ds.headerFn())
			offsite := CmdSize + LengthSize + HashSize + BlockNumberSize
			totalLength := offsite + ForkNumberSize*len(ds.forks)
			if totalLength > len(buffer) {
				buffer = append(buffer, make([]byte, totalLength-len(buffer))...)
			}
			for i := 0; i < len(ds.forks); i++ {
				binary.LittleEndian.PutUint64(buffer[offsite+i*ForkNumberSize:], ds.forks[i])
			}

			_, err = conn.Write(buffer[:totalLength])
			if err != nil {
				log.Warn(SendFailedMsg, "reason", err)
			}
		case GetGenesisMsg:
			// ============================================
			// 4 bytes: command of message
			// 32 bytes: genesis hash
			copy(buffer, GetGenesisMsg)
			copy(buffer[CmdSize:], ds.genesisHash[:])

			_, err = conn.Write(buffer[:CmdSize+HashSize])
			if err != nil {
				log.Warn(SendFailedMsg, "reason", err)
			}
		case GetBlockNumber:
			// ============================================
			// 4 bytes: command of message
			// 8 bytes: block number (little endian)
			copy(buffer, GetBlockNumber)
			binary.LittleEndian.PutUint64(buffer[CmdSize:], ds.headerFn())

			_, err = conn.Write(buffer[:CmdSize+BlockNumberSize])
			if err != nil {
				log.Warn(SendFailedMsg, "reason", err)
			}
		case GetForksMsg:
			// ============================================
			// 4 bytes: command of message
			// 4 bytes: length of forks (little endian)
			// (many) 8 bytes: fork
			copy(buffer, GetForksMsg)
			binary.LittleEndian.PutUint32(buffer[CmdSize:], uint32(len(ds.forks)))
			offsite := CmdSize + LengthSize
			totalLength := CmdSize + LengthSize + ForkNumberSize*len(ds.forks)
			for i := 0; i < len(ds.forks); i++ {
				binary.LittleEndian.PutUint64(buffer[offsite+i*ForkNumberSize:], ds.forks[i])
			}

			_, err = conn.Write(buffer[:totalLength])
			if err != nil {
				log.Warn(SendFailedMsg, "reason", err)
			}
		case GetHeaderByHash:
			err = ds.serveGetHeader(cmd, rdr, wdr)
			if err != nil {
				log.Warn("serve GetHeaderByHash failed", "reason", err)
			}
		case GetHeaderByNumber:
			err = ds.serveGetHeader(cmd, rdr, wdr)
			if err != nil {
				log.Warn("serve GetHeader failed", "reason", err)
			}
		case GetHeaderByHashAndNumber:
			err = ds.serveGetHeader(cmd, rdr, wdr)
			if err != nil {
				log.Warn("serve GetHeaderByHashAndNumber failed", "reason", err)
			}
		case GetAncestor:
			err = ds.serveGetAncestor(rdr, wdr)
			if err != nil {
				log.Warn("serve GetAncestor failed", "reason", err)
			}
		case GetBlockBodyRlpByHash:
			err = ds.serveGetBlockBodyRlpByHash(rdr, wdr)
			if err != nil {
				log.Warn("serve GetBlockBodyRlpByHash failed", "reason", err)
			}
		case GetReceiptsByHash:
			err = ds.serveGetReceiptsByHash(rdr, wdr)
			if err != nil {
				log.Warn("serve GetReceiptsByHash failed", "reason", err)
			}
		case GetContractCodeWithPrefix:
			err = ds.serveGetContractCodeWithPrefix(rdr, wdr)
			if err != nil {
				log.Warn("serve GetContractCodeWithPrefix failed", "reason", err)
			}
		case GetTrieNode:
			err = ds.serveGetTrieNode(rdr, wdr)
			if err != nil {
				log.Warn("serve GetTrieNode failed", "reason", err)
			}
		case GetTotalDifficulty:
			err = ds.serveGetTotalDifficulty(rdr, wdr)
			if err != nil {
				log.Warn("serve GetTotalDifficulty failed", "reason", err)
			}
		case GetValidateHeader:
			err = ds.serverValidateHeader(rdr, wdr)
			if err != nil {
				log.Warn("serve GetValidateHeader failed", "reason", err)
			}
		default:
			log.Warn("invalid request from disguise client")
		}

		if err == nil {
			_, err = io.ReadFull(rdr, buffer[:CmdSize])
		}
	}
	atomic.AddInt32(&ds.curConCount, -1)
	conn.Close()
	log.Info("close disguise client connection", "connection count", atomic.LoadInt32(&ds.curConCount))
}

func (ds *DisguiseServer) serveGetTotalDifficulty(rdr *bufio.Reader, wdr *bufio.Writer) error {
	var (
		err       error
		buffer    = make([]byte, 0x1000)
		resHash   common.Hash
		resNumber uint64
		td        *big.Int
	)
	_, err = io.ReadFull(rdr, buffer[:HashSize+BlockNumberSize])
	if err != nil {
		return err
	}

	resHash.SetBytes(buffer[:HashSize])
	resNumber = binary.LittleEndian.Uint64(buffer[HashSize : HashSize+BlockNumberSize])
	td = ds.chain.GetTd(resHash, resNumber)
	if td == nil {
		header := ds.chain.CurrentHeader()
		if td = ds.chain.GetTd(header.Hash(), header.Number.Uint64()); td == nil {
			td = big.NewInt(0)
		}
	}

	binary.LittleEndian.PutUint32(buffer, BlockNumberSize)
	binary.LittleEndian.PutUint64(buffer[LengthSize:], td.Uint64())
	_, err = wdr.Write(buffer[:LengthSize+BlockNumberSize])
	_ = wdr.Flush()
	if err != nil {
		return err
	}
	return nil
}

func (ds *DisguiseServer) serveGetTrieNode(rdr *bufio.Reader, wdr *bufio.Writer) error {
	var (
		err     error
		buffer  = make([]byte, 0x1000)
		resHash common.Hash
		data    []byte
	)
	_, err = io.ReadFull(rdr, buffer[:HashSize])
	if err != nil {
		return err
	}
	resHash.SetBytes(buffer[:HashSize])
	data, err = ds.chain.TrieNode(resHash)
	if err != nil {
		data = data[:0]
	}
	binary.LittleEndian.PutUint32(buffer, uint32(len(data)))
	copy(buffer[LengthSize:], data)
	_, err = wdr.Write(buffer[:LengthSize+len(data)])
	_ = wdr.Flush()
	if err != nil {
		return err
	}
	return nil
}

func (ds *DisguiseServer) serveGetContractCodeWithPrefix(rdr *bufio.Reader, wdr *bufio.Writer) error {
	var (
		err     error
		buffer  = make([]byte, 0x1000)
		resHash common.Hash
		data    []byte
	)
	_, err = io.ReadFull(rdr, buffer[:HashSize])
	if err != nil {
		return err
	}
	resHash.SetBytes(buffer[:HashSize])
	data, err = ds.chain.ContractCodeWithPrefix(resHash)
	if err != nil {
		data = data[:0]
	}
	binary.LittleEndian.PutUint32(buffer, uint32(len(data)))
	copy(buffer[LengthSize:], data)
	_, err = wdr.Write(buffer[:LengthSize+len(data)])
	_ = wdr.Flush()
	if err != nil {
		return err
	}
	return nil
}

func (ds *DisguiseServer) serveGetReceiptsByHash(rdr *bufio.Reader, wdr *bufio.Writer) error {
	var (
		err           error
		buffer        = make([]byte, 0x1000)
		resHash       common.Hash
		receipts      types.Receipts
		receiptsBytes []byte
	)
	_, err = io.ReadFull(rdr, buffer[:HashSize])
	if err != nil {
		return err
	}
	resHash.SetBytes(buffer[:HashSize])
	receipts = ds.chain.GetReceiptsByHash(resHash)
	receiptsBytes, err = rlp.EncodeToBytes(receipts)
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(buffer, uint32(len(receiptsBytes)))
	copy(buffer[LengthSize:], receiptsBytes)
	_, err = wdr.Write(buffer[:LengthSize+len(receiptsBytes)])
	_ = wdr.Flush()
	if err != nil {
		return err
	}
	return nil
}

func (ds *DisguiseServer) serveGetBlockBodyRlpByHash(rdr *bufio.Reader, wdr *bufio.Writer) error {
	var (
		err     error
		buffer  = make([]byte, 0x10000)
		resHash common.Hash
		data    rlp.RawValue
	)
	_, err = io.ReadFull(rdr, buffer[:HashSize])
	if err != nil {
		return err
	}
	resHash.SetBytes(buffer[:HashSize])
	data = ds.chain.GetBodyRLP(resHash)
	if LengthSize+len(data) > len(buffer) {
		buffer = make([]byte, LengthSize+len(data))
	}

	binary.LittleEndian.PutUint32(buffer, uint32(len(data)))
	copy(buffer[LengthSize:], data)
	_, err = wdr.Write(buffer[:LengthSize+len(data)])
	_ = wdr.Flush()
	if err != nil {
		return err
	}
	return nil
}

func (ds *DisguiseServer) serveGetAncestor(rdr *bufio.Reader, wdr *bufio.Writer) error {
	var (
		err             error
		buffer          = make([]byte, 0x1000)
		resHash         common.Hash
		resNumber       uint64
		resAncestor     uint64
		resNonCanonical uint64
		ansHash         common.Hash
		ansUint64       uint64
	)
	_, err = io.ReadFull(rdr, buffer[:HashSize+BlockNumberSize*3])
	if err != nil {
		return err
	}
	resHash.SetBytes(buffer[:HashSize])
	resNumber = binary.LittleEndian.Uint64(buffer[HashSize : HashSize+BlockNumberSize])
	resAncestor = binary.LittleEndian.Uint64(buffer[HashSize+BlockNumberSize : HashSize+BlockNumberSize*2])
	resNonCanonical = binary.LittleEndian.Uint64(buffer[HashSize+BlockNumberSize*2 : HashSize+BlockNumberSize*3])

	ansHash, ansUint64 = ds.chain.GetAncestor(resHash, resNumber, resAncestor, &resNonCanonical)
	binary.LittleEndian.PutUint32(buffer, HashSize+BlockNumberSize)
	copy(buffer[LengthSize:], ansHash.Bytes())
	binary.LittleEndian.PutUint64(buffer[LengthSize+HashSize:], ansUint64)

	_, err = wdr.Write(buffer[:LengthSize+HashSize+BlockNumberSize])
	_ = wdr.Flush()
	if err != nil {
		return err
	}
	return nil
}

func (ds *DisguiseServer) serverValidateHeader(rdr *bufio.Reader, wdr *bufio.Writer) error {
	var (
		err    error
		buffer = make([]byte, 0x1000)
		num    uint32
		header = &types.Header{}
	)
	_, err = io.ReadFull(rdr, buffer[:LengthSize])
	if err != nil {
		return err
	}
	num = binary.LittleEndian.Uint32(buffer[:LengthSize])
	if num > uint32(len(buffer)) {
		buffer = make([]byte, num+1)
	}
	_, err = io.ReadFull(rdr, buffer[:num])
	if err != nil {
		return err
	}
	err = rlp.DecodeBytes(buffer[:num], header)
	if err != nil {
		return err
	}

	if ds.chain.CurrentHeader().Number.Cmp(header.Number) < 0 {
		num = 0
	} else {
		err = ds.chain.Engine().VerifyHeader(ds.chain, header, true)
		if err != nil {
			num = 1
		} else {
			num = 0
		}
	}
	binary.LittleEndian.PutUint32(buffer[:LengthSize], num)
	_, err = wdr.Write(buffer[:LengthSize])
	_ = wdr.Flush()
	if err != nil {
		return err
	}
	return nil
}

func (ds *DisguiseServer) serveGetHeader(cmd string, rdr *bufio.Reader, wdr *bufio.Writer) error {
	var (
		err         error
		bytesN      int
		buffer      = make([]byte, 0x1000)
		resHash     common.Hash
		resNumber   uint64
		header      *types.Header = nil
		headerBytes []byte
	)
	switch cmd {
	case GetHeaderByHash:
		bytesN, err = io.ReadFull(rdr, buffer[:HashSize])
		if err != nil || bytesN != HashSize {
			return err
		}
		resHash.SetBytes(buffer[:HashSize])
		header = ds.chain.GetHeaderByHash(resHash)
	case GetHeaderByNumber:
		bytesN, err = io.ReadFull(rdr, buffer[:BlockNumberSize])
		if err != nil || bytesN != BlockNumberSize {
			return err
		}
		resNumber = binary.LittleEndian.Uint64(buffer[:BlockNumberSize])
		header = ds.chain.GetHeaderByNumber(resNumber)
	case GetHeaderByHashAndNumber:
		bytesN, err = io.ReadFull(rdr, buffer[:HashSize+BlockNumberSize])
		if err != nil || bytesN != HashSize+BlockNumberSize {
			return err
		}
		resHash.SetBytes(buffer[:HashSize])
		resNumber = binary.LittleEndian.Uint64(buffer[HashSize : HashSize+BlockNumberSize])
		header = ds.chain.GetHeader(resHash, resNumber)
	}

	headerBytes, err = rlp.EncodeToBytes(header)
	if err != nil {
		log.Warn("encode header to rlp failed", "reason", err)
		return err
	}

	binary.LittleEndian.PutUint32(buffer, uint32(len(headerBytes)))
	copy(buffer[LengthSize:], headerBytes)

	_, err = wdr.Write(buffer[:LengthSize+len(headerBytes)])
	_ = wdr.Flush()
	if err != nil {
		return err
	}
	return nil
}
