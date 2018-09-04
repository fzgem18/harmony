package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/simple-rules/harmony-benchmark/blockchain"
	"github.com/simple-rules/harmony-benchmark/client"
	client_config "github.com/simple-rules/harmony-benchmark/client/config"
	"github.com/simple-rules/harmony-benchmark/consensus"
	"github.com/simple-rules/harmony-benchmark/crypto/pki"
	"github.com/simple-rules/harmony-benchmark/log"
	"github.com/simple-rules/harmony-benchmark/node"
	"github.com/simple-rules/harmony-benchmark/p2p"
	proto_node "github.com/simple-rules/harmony-benchmark/proto/node"
)

type txGenSettings struct {
	numOfAddress      int
	crossShard        bool
	maxNumTxsPerBatch int
}

var (
	utxoPoolMutex sync.Mutex
	setting       txGenSettings
)

type TxInfo struct {
	// Global Input
	shardID   int
	dataNodes []*node.Node
	// Temp Input
	id      [32]byte
	index   uint32
	value   int
	address [20]byte
	// Output
	txs      []*blockchain.Transaction
	crossTxs []*blockchain.Transaction
	txCount  int
}

// Generates at most "maxNumTxs" number of simulated transactions based on the current UtxoPools of all shards.
// The transactions are generated by going through the existing utxos and
// randomly select a subset of them as the input for each new transaction. The output
// address of the new transaction are randomly selected from [0 - N), where N is the total number of fake addresses.
//
// When crossShard=true, besides the selected utxo input, select another valid utxo as input from the same address in a second shard.
// Similarly, generate another utxo output in that second shard.
//
// NOTE: the genesis block should contain N coinbase transactions which add
//       token (1000) to each address in [0 - N). See node.AddTestingAddresses()
//
// Params:
//     shardID                    - the shardID for current shard
//     dataNodes                  - nodes containing utxopools of all shards
// Returns:
//     all single-shard txs
//     all cross-shard txs
func generateSimulatedTransactions(shardID int, dataNodes []*node.Node) ([]*blockchain.Transaction, []*blockchain.Transaction) {
	/*
	  UTXO map structure:
	     address - [
	                txId1 - [
	                        outputIndex1 - value1
	                        outputIndex2 - value2
	                       ]
	                txId2 - [
	                        outputIndex1 - value1
	                        outputIndex2 - value2
	                       ]
	               ]
	*/

	utxoPoolMutex.Lock()
	txInfo := TxInfo{}
	txInfo.shardID = shardID
	txInfo.dataNodes = dataNodes
	txInfo.txCount = 0

UTXOLOOP:
	// Loop over all addresses
	for address, txMap := range dataNodes[shardID].UtxoPool.UtxoMap {
		txInfo.address = address
		// Loop over all txIds for the address
		for txIdStr, utxoMap := range txMap {
			// Parse TxId
			id, err := hex.DecodeString(txIdStr)
			if err != nil {
				continue
			}
			copy(txInfo.id[:], id[:])

			// Loop over all utxos for the txId
			for index, value := range utxoMap {
				txInfo.index = index
				txInfo.value = value

				randNum := rand.Intn(100)
				// 30% sample rate to select UTXO to use for new transactions
				if randNum >= 30 {
					continue
				}
				if setting.crossShard && randNum < 10 { // 1/3 cross shard transactions: add another txinput from another shard
					generateCrossShardTx(&txInfo)
				} else {
					generateSingleShardTx(&txInfo)
				}
				if txInfo.txCount >= setting.maxNumTxsPerBatch {
					break UTXOLOOP
				}
			}
		}
	}
	utxoPoolMutex.Unlock()

	log.Debug("[Generator] generated transations", "single-shard", len(txInfo.txs), "cross-shard", len(txInfo.crossTxs))
	return txInfo.txs, txInfo.crossTxs
}

func generateCrossShardTx(txInfo *TxInfo) {
	nodeShardID := txInfo.dataNodes[txInfo.shardID].Consensus.ShardID
	// shard with neighboring Id
	crossShardId := (int(nodeShardID) + 1) % len(txInfo.dataNodes)

	crossShardNode := txInfo.dataNodes[crossShardId]
	crossShardUtxosMap := crossShardNode.UtxoPool.UtxoMap[txInfo.address]

	// Get the cross shard utxo from another shard
	var crossTxin *blockchain.TXInput
	crossUtxoValue := 0
	// Loop over utxos for the same address from the other shard and use the first utxo as the second cross tx input
	for crossTxIdStr, crossShardUtxos := range crossShardUtxosMap {
		// Parse TxId
		id, err := hex.DecodeString(crossTxIdStr)
		if err != nil {
			continue
		}
		crossTxId := [32]byte{}
		copy(crossTxId[:], id[:])

		for crossShardIndex, crossShardValue := range crossShardUtxos {
			crossUtxoValue = crossShardValue
			crossTxin = blockchain.NewTXInput(blockchain.NewOutPoint(&crossTxId, crossShardIndex), txInfo.address, uint32(crossShardId))
			break
		}
		if crossTxin != nil {
			break
		}
	}

	// Add the utxo from current shard
	txIn := blockchain.NewTXInput(blockchain.NewOutPoint(&txInfo.id, txInfo.index), txInfo.address, nodeShardID)
	txInputs := []blockchain.TXInput{*txIn}

	// Add the utxo from the other shard, if any
	if crossTxin != nil { // This means the ratio of cross shard tx could be lower than 1/3
		txInputs = append(txInputs, *crossTxin)
	}

	// Spend the utxo from the current shard to a random address in [0 - N)
	txout := blockchain.TXOutput{Amount: txInfo.value, Address: pki.GetAddressFromInt(rand.Intn(setting.numOfAddress) + 1), ShardID: nodeShardID}

	txOutputs := []blockchain.TXOutput{txout}

	// Spend the utxo from the other shard, if any, to a random address in [0 - N)
	if crossTxin != nil {
		crossTxout := blockchain.TXOutput{Amount: crossUtxoValue, Address: pki.GetAddressFromInt(rand.Intn(setting.numOfAddress) + 1), ShardID: uint32(crossShardId)}
		txOutputs = append(txOutputs, crossTxout)
	}

	// Construct the new transaction
	tx := blockchain.Transaction{ID: [32]byte{}, TxInput: txInputs, TxOutput: txOutputs, Proofs: nil}

	priKeyInt, ok := client.LookUpIntPriKey(txInfo.address)
	if ok {
		tx.PublicKey = pki.GetBytesFromPublicKey(pki.GetPublicKeyFromScalar(pki.GetPrivateKeyScalarFromInt(priKeyInt)))

		tx.SetID() // TODO(RJ): figure out the correct way to set Tx ID.
		tx.Sign(pki.GetPrivateKeyScalarFromInt(priKeyInt))
	} else {
		log.Error("Failed to look up the corresponding private key from address", "Address", txInfo.address)
		return
	}

	txInfo.crossTxs = append(txInfo.crossTxs, &tx)
	txInfo.txCount++
}

func generateSingleShardTx(txInfo *TxInfo) {
	nodeShardID := txInfo.dataNodes[txInfo.shardID].Consensus.ShardID
	// Add the utxo as new tx input
	txin := blockchain.NewTXInput(blockchain.NewOutPoint(&txInfo.id, txInfo.index), txInfo.address, nodeShardID)

	// Spend the utxo to a random address in [0 - N)
	txout := blockchain.TXOutput{Amount: txInfo.value, Address: pki.GetAddressFromInt(rand.Intn(setting.numOfAddress) + 1), ShardID: nodeShardID}
	tx := blockchain.Transaction{ID: [32]byte{}, TxInput: []blockchain.TXInput{*txin}, TxOutput: []blockchain.TXOutput{txout}, Proofs: nil}

	priKeyInt, ok := client.LookUpIntPriKey(txInfo.address)
	if ok {
		tx.PublicKey = pki.GetBytesFromPublicKey(pki.GetPublicKeyFromScalar(pki.GetPrivateKeyScalarFromInt(priKeyInt)))
		tx.SetID() // TODO(RJ): figure out the correct way to set Tx ID.
		tx.Sign(pki.GetPrivateKeyScalarFromInt(priKeyInt))
	} else {
		log.Error("Failed to look up the corresponding private key from address", "Address", txInfo.address)
		return
	}

	txInfo.txs = append(txInfo.txs, &tx)
	txInfo.txCount++
}

// A utility func that counts the total number of utxos in a pool.
func countNumOfUtxos(utxoPool *blockchain.UTXOPool) int {
	countAll := 0
	for _, utxoMap := range utxoPool.UtxoMap {
		for txIdStr, val := range utxoMap {
			_ = val
			id, err := hex.DecodeString(txIdStr)
			if err != nil {
				continue
			}

			txId := [32]byte{}
			copy(txId[:], id[:])
			for _, utxo := range val {
				_ = utxo
				countAll++
			}
		}
	}
	return countAll
}

func main() {
	configFile := flag.String("config_file", "local_config.txt", "file containing all ip addresses and config")
	maxNumTxsPerBatch := flag.Int("max_num_txs_per_batch", 100000, "number of transactions to send per message")
	logFolder := flag.String("log_folder", "latest", "the folder collecting the logs of this execution")
	flag.Parse()

	// Read the configs
	config := client_config.NewConfig()
	config.ReadConfigFile(*configFile)
	leaders, shardIds := config.GetLeadersAndShardIds()

	setting.numOfAddress = 10000
	// Do cross shard tx if there are more than one shard
	setting.crossShard = len(shardIds) > 1
	setting.maxNumTxsPerBatch = *maxNumTxsPerBatch

	// TODO(Richard): refactor this chuck to a single method
	// Setup a logger to stdout and log file.
	logFileName := fmt.Sprintf("./%v/txgen.log", *logFolder)
	h := log.MultiHandler(
		log.StdoutHandler,
		log.Must.FileHandler(logFileName, log.LogfmtFormat()), // Log to file
	)
	log.Root().SetHandler(h)

	// Nodes containing utxopools to mirror the shards' data in the network
	nodes := []*node.Node{}
	for _, shardId := range shardIds {
		node := node.New(&consensus.Consensus{ShardID: shardId}, nil)
		// Assign many fake addresses so we have enough address to play with at first
		node.AddTestingAddresses(setting.numOfAddress)
		nodes = append(nodes, node)
	}

	// Client/txgenerator server node setup
	clientPort := config.GetClientPort()
	consensusObj := consensus.NewConsensus("0", clientPort, "0", nil, p2p.Peer{})
	clientNode := node.New(consensusObj, nil)

	if clientPort != "" {
		clientNode.Client = client.NewClient(&leaders)

		// This func is used to update the client's utxopool when new blocks are received from the leaders
		updateBlocksFunc := func(blocks []*blockchain.Block) {
			log.Debug("Received new block from leader", "len", len(blocks))
			for _, block := range blocks {
				for _, node := range nodes {
					if node.Consensus.ShardID == block.ShardId {
						log.Debug("Adding block from leader", "shardId", block.ShardId)
						// Add it to blockchain
						utxoPoolMutex.Lock()
						node.AddNewBlock(block)
						utxoPoolMutex.Unlock()
					} else {
						continue
					}
				}
			}
		}
		clientNode.Client.UpdateBlocks = updateBlocksFunc

		// Start the client server to listen to leader's message
		go func() {
			clientNode.StartServer(clientPort)
		}()

	}

	// Transaction generation process
	time.Sleep(10 * time.Second) // wait for nodes to be ready
	start := time.Now()
	totalTime := 60.0 //run for 1 minutes

	for true {
		t := time.Now()
		if t.Sub(start).Seconds() >= totalTime {
			log.Debug("Generator timer ended.", "duration", (int(t.Sub(start))), "startTime", start, "totalTime", totalTime)
			break
		}

		allCrossTxs := []*blockchain.Transaction{}
		// Generate simulated transactions
		for i, leader := range leaders {
			txs, crossTxs := generateSimulatedTransactions(i, nodes)
			allCrossTxs = append(allCrossTxs, crossTxs...)

			log.Debug("[Generator] Sending single-shard txs ...", "leader", leader, "numTxs", len(txs), "numCrossTxs", len(crossTxs))
			msg := proto_node.ConstructTransactionListMessage(txs)
			p2p.SendMessage(leader, msg)
			// Note cross shard txs are later sent in batch
		}

		if len(allCrossTxs) > 0 {
			log.Debug("[Generator] Broadcasting cross-shard txs ...", "allCrossTxs", len(allCrossTxs))
			msg := proto_node.ConstructTransactionListMessage(allCrossTxs)
			p2p.BroadcastMessage(leaders, msg)

			// Put cross shard tx into a pending list waiting for proofs from leaders
			if clientPort != "" {
				clientNode.Client.PendingCrossTxsMutex.Lock()
				for _, tx := range allCrossTxs {
					clientNode.Client.PendingCrossTxs[tx.ID] = tx
				}
				clientNode.Client.PendingCrossTxsMutex.Unlock()
			}
		}

		time.Sleep(500 * time.Millisecond) // Send a batch of transactions periodically
	}

	// Send a stop message to stop the nodes at the end
	msg := proto_node.ConstructStopMessage()
	peers := append(config.GetValidators(), leaders...)
	p2p.BroadcastMessage(peers, msg)
}
