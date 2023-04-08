# BNB Smart Chain: Latency Optimized Client

### 1. 📊 What is mempool

Mempool is a data container that stores the some valid but unconfirmed transactions waiting to be included in next blocks. Once a user cryptographically signs a transaction, the transaction is sent to the p2p network where it is broadcasted using mixed push and pull to all connected peers. Each peer that receives the transaction adds it to their mempool. 

Every peer maintains its own mempool, and the transactions in it may differ due to propagation delays, peer connectivity. Miners select a set of transactions to include in the next block, those transactions are removed from the mempool. Transactions that are not included in a block remain in the mempool and are considered pending transactions.

### 2. 🕰️ Why Latency is important to MEV

Take cyclic arbitrage as an example, to perform successful arbitrage, the arbitrageur must often make a back-running transaction `b`  immediately after the target transaction `m`. Techniques for achieving this goal have shifted over time.

For bnb smart chain, arbitrage happened mostly via back-running, in which arbitrageurs would observe a victim transaction, then publicly broadcast backrunning transactions. Low latency implies swifter reactions to send arbitrage transactions, and hence more opportunities sends this transaction to miners first.

In 2022, the mechanisms for arbitrage began to shift to private auction channels like `BNB48`, in which an arbitrageur submits a miner-extractable-value (MEV) bundle with a tip for miners to a public relay that privately forwards the bundle to miners. Assuming that every arbitrageur is paying the miner with the same maximum tip, the competition collapses to one of speed.

### 3. 🧐 What the mechanism behind this client

The client has two maps to store information about transactions and blocks received from neighbors. Every certain time interval, the relay node computes scores for each neighbor based on the arrival times of transactions and blocks in the past interval. 

Our scores function returns a score list for each neighbor. It selects the best neighbors by sorting the score lists and selecting the smallest entries. The union set of these two neighbor sets becomes the best neighbors to keep. IPs of peers not in this set are added to the blocklist. If the peer appears in the blocklist `n` times before, an expiration time of `2^n*20` minutes is set as a penalty. The blocked IP address will become available again after expiration and can be reconnected if needed.

We implemented the mechanism in the [file](./eth/peri.go).

### 4. 💻 How to use it

In terms of usage, there is no difference between using this modified geth client and the regular one. The only difference is that you can see the peer selection process in the log files.

