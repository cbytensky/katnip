package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"math/bits"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/bmatsuo/lmdb-go/lmdb"
	"github.com/kaspanet/kaspad/app/appmessage"
	"github.com/kaspanet/kaspad/domain/consensus/utils/constants"
	"github.com/kaspanet/kaspad/infrastructure/network/rpcclient"
)

const KeyLength = 16 // length of part of hash that is used as DB keys (it is not necessary to use all of 32 bytes)
const ProgName = "Katnip"

// These byte values are used as first bytes of DB keys to indicate type of data
const (
	PrefixBlock byte = iota
	PrefixBlockChild
	PrefixBlueScoreBlock
	PrefixTransaction
	PrefixTransactionHash
	PrefixTransactionBlock
	PrefixAddress
	PrefixBluestBlock
	PrefixPruningPoints
	PrefixBlockDagInfo
	PrefixTransactionSpent
)

// types that is used in blockDAG structs
type (
	Hash       [32]byte
	UInt64Full uint64          // is used to distinguish from uint values that is stored in compact form
	Key        [KeyLength]byte // DB hash key type
	Bytes      []byte
)

// structs of blockDAG elements
type (
	Block struct {
		Hash               Hash
		IsHeaderOnly       bool
		BlueScore          uint64
		Version            uint32
		SelectedParent     uint64
		Parents            []Hash
		ParentLevels       [][]uint64
		MerkleRoot         Hash
		AcceptedMerkleRoot Hash
		UTXOCommitment     Hash
		Timestamp          uint64
		Bits               uint32
		Nonce              UInt64Full
		DAAScore           uint64
		BlueWork           Bytes
		PruningPoint       uint64
		TransactionIds     []Hash
	}

	Transaction struct {
		Hash      Hash
		Id        Hash
		Version   uint16
		LockTime  uint64
		Payload   Bytes
		ExtraData string
		Mass      uint64
		Inputs    []Input
		Outputs   []Output
	}

	Input struct {
		Sequence              uint64
		PreviousTransactionID Hash
		PreviousIndex         uint32
		SignatureScript       Bytes
		SignatureOpCount      byte
	}

	Output struct {
		Amount                 uint64
		ScriptPublicKeyVersion uint16
		ScriptPublicKey        Bytes
		ScriptPublicKeyType    string
		ScriptPublicKeyAddress string
	}

	TransactionBlock struct {
		TransactionKey Key
		BlockKey       Key
	}

	TransactionSpent struct {
		TransactionKey      Key
		SpentTransactionKey Key
	}

	AddressTransaction struct {
		Address       string
		TransactionId Key
	}

	BlockDAGInfo struct {
		KaspadVersion       string
		BlockCount          uint64
		HeaderCount         uint64
		TipHashes           []Hash
		VirtualParentHashes []Hash
		Difficulty          float64
		PastMedianTime      uint64
		PruningPointIndex   uint64
		VirtualDAAScore     uint64
		LatestHashes        [NumLatestHashes]Hash
		LatestHashesTop     uint64
		CirculatingSupply   UInt64Full
	}
)

type RpcBlocks []*appmessage.RPCBlock

type WriteChanElem struct {
	RpcBlocks         RpcBlocks
	BluestHash        *Hash
	RpcBlockDagInfo   *appmessage.GetBlockDAGInfoResponseMessage
	CirculatingSupply uint64
}

var DbEnv *lmdb.Env
var Db lmdb.DBI

var WriteChan = make(chan WriteChanElem, 10)

var MaxBlueWorkStr string
var BluestHashStr string
var PruningPointsStr = make(map[string]uint64, 1)

const NumLatestHashes = 20

var LatestHashes [NumLatestHashes]*Hash
var LatestHashesTop int
var RpcClient *rpcclient.RPCClient
var KaspadVersion string

func main() {
	dirName, err := os.UserHomeDir()
	PanicIfErr(err)
	defaultDbFileName := dirName + "/" + strings.ToLower(ProgName) + ".mdb"

	// Process command line arguments
	rpcServerAddr := flag.String("rpcserver", "0.0.0.0:16110", "Kaspa RPC server address")
	dbFileName := flag.String("dbfile", defaultDbFileName, "Database file name")
	AddFlagHttp()
	AddFlagLog()
	flag.Parse()
	if len(flag.Args()) > 0 {
		flag.Usage()
		os.Exit(1)
	}
	Log(LogInf, "Log level: %d", *LogLevel)

	go HttpServe()

	DbEnv, err = lmdb.NewEnv()
	PanicIfErr(err)
	PanicIfErr(DbEnv.SetMapSize(1 << 26)) // 1 GB
	Log(LogInf, "Database file: %s", *dbFileName)
	PanicIfErr(DbEnv.Open(*dbFileName, lmdb.NoSubdir|lmdb.WriteMap|lmdb.NoMetaSync|lmdb.NoSync|lmdb.MapAsync|lmdb.NoLock|lmdb.NoMemInit, 0644))
	PanicIfErr(DbEnv.Update(func(txn *lmdb.Txn) (err error) {
		Db, err = txn.OpenRoot(0)
		return err
	}))

	go InsertingToDb()

	for {
		Log(LogInf, "Connecting to KaspaD: %s", *rpcServerAddr)
		RpcClient, err = rpcclient.NewRPCClient(*rpcServerAddr)
		if err == nil {
			break
		}
		Log(LogErr, "%v", err)
	}
	Log(LogInf, "Connected to KaspaD: %s", *rpcServerAddr)
	info, err := RpcClient.GetInfo()
	PanicIfErr(err)
	KaspadVersion = info.ServerVersion
	//if info.Banner != "" {
	//	Log(LogInf, "Kaspa node’s banner: %s", info.Banner)
	//}

	idbStartTime := time.Now()
	Log(LogInf, "IBD stared")

	var bluestHash Hash
	if dbGet(PrefixBluestBlock, nil, &bluestHash) {
		BluestHashStr = H2s(bluestHash)
		Log(LogInf, "Stored bluest block: %s", BluestHashStr)
	} else {
		Log(LogInf, "Bluest block not stored")
	}

	_ = DbEnv.View(func(txn *lmdb.Txn) (err error) {
		cursor, err := txn.OpenCursor(Db)
		PanicIfErr(err)
		keyPrefix := []byte{PrefixPruningPoints}
		key, val, err := cursor.Get([]byte{PrefixPruningPoints}, nil, lmdb.SetRange)
		if lmdb.IsErrno(err, lmdb.NotFound) {
			Log(LogInf, "Pruning points not stored")
			return nil
		}
		PanicIfErr(err)
		for {
			equal := true
			for i, v := range keyPrefix {
				if key[i] != v {
					equal = false
					break
				}
			}
			if !equal {
				break
			}
			var pruningPointIndex uint64
			var pruningPoint Hash
			SerializeValue(false, bytes.NewBuffer(key[1:]), reflect.ValueOf(&pruningPointIndex).Elem())
			SerializeValue(false, bytes.NewBuffer(val), reflect.ValueOf(&pruningPoint).Elem())
			pruningPointStr := H2s(pruningPoint)
			PruningPointsStr[pruningPointStr] = pruningPointIndex
			Log(LogInf, "Stored pruning point %d: %s", pruningPointIndex, pruningPointStr)
			key, val, err = cursor.Get(nil, nil, lmdb.Next)
			if lmdb.IsErrno(err, lmdb.NotFound) {
				break
			}
			PanicIfErr(err)
		}
		return nil
	})
	ibdCount := 0
	if BluestHashStr == "" {
		bdi, err := RpcClient.GetBlockDAGInfo()
		PanicIfErr(err)
		BluestHashStr = bdi.PruningPointHash
	}
	for {
		Log(LogDbg, "getBlocks: %s", BluestHashStr)
		response, err := RpcClient.GetBlocks(BluestHashStr, true, true)
		if err != nil {
			Log(LogErr, "%v", err)
			continue
		}
		rpcBlocks := response.Blocks
		numBlocks := len(rpcBlocks) - 1
		ibdCount += numBlocks
		bluestHashStrLast := BluestHashStr
		AddToWriteChan(rpcBlocks)
		if bluestHashStrLast == BluestHashStr {
			break
		}
		Log(LogInf, "Inserting blocks: %3d, %s", numBlocks, FormatTimestamp(uint64(rpcBlocks[len(rpcBlocks)-1].Header.Timestamp)))
	}
	duration := time.Since(idbStartTime).Round(time.Second)
	seconds := int(duration.Seconds())
	bps := 0
	if seconds != 0 {
		bps = ibdCount / seconds
	}
	PanicIfErr(DbEnv.Sync(true))
	Log(LogInf, "IBD finished: %d blocks, %s, %d bps", ibdCount, duration, bps)

	PanicIfErr(RpcClient.RegisterForBlockAddedNotifications(func(notification *appmessage.BlockAddedNotificationMessage) {
		hashStr := notification.Block.VerboseData.Hash
		blockResponse, err := RpcClient.GetBlock(hashStr, true)
		PanicIfErr(err)
		AddToWriteChan(RpcBlocks{blockResponse.Block})
		Log(LogInf, "Added block: %s", hashStr)
	}))

	select {}
}

func AddToWriteChan(rpcBlocks RpcBlocks) {
	rpcBlockDAGInfo, err := RpcClient.GetBlockDAGInfo()
	PanicIfErr(err)
	rpcCoinSupply, err := RpcClient.GetCoinSupply()
	PanicIfErr(err)
	var bluestHashToWrite *Hash
	for _, rpcBlock := range rpcBlocks {
		blueWorkStr := rpcBlock.Header.BlueWork
		if len(blueWorkStr) > len(MaxBlueWorkStr) || len(blueWorkStr) == len(MaxBlueWorkStr) && blueWorkStr > MaxBlueWorkStr {
			MaxBlueWorkStr = blueWorkStr
			BluestHashStr = rpcBlock.VerboseData.Hash
			bluestHash := S2h(BluestHashStr)
			bluestHashToWrite = &bluestHash
		}
	}
	WriteChan <- WriteChanElem{rpcBlocks, bluestHashToWrite, rpcBlockDAGInfo, rpcCoinSupply.CirculatingSompi}
}

func InsertingToDb() {
	Log(LogInf, "Writing to DB routine started")
	for {
		writeElem := <-WriteChan
		rpcBlocks := writeElem.RpcBlocks
		for {
			err := DbEnv.Update(func(txn *lmdb.Txn) (err error) {
				for _, rpcBlock := range rpcBlocks {
					rpcVData := rpcBlock.VerboseData

					rpcHeader := rpcBlock.Header
					blockHash := S2h(rpcVData.Hash)
					blueWork := S2b(rpcHeader.BlueWork)

					block := Block{
						Hash:               blockHash,
						IsHeaderOnly:       rpcVData.IsHeaderOnly,
						BlueScore:          rpcVData.BlueScore,
						Version:            rpcHeader.Version,
						MerkleRoot:         S2h(rpcHeader.HashMerkleRoot),
						AcceptedMerkleRoot: S2h(rpcHeader.AcceptedIDMerkleRoot),
						UTXOCommitment:     S2h(rpcHeader.UTXOCommitment),
						Timestamp:          uint64(rpcHeader.Timestamp),
						Bits:               rpcHeader.Bits,
						Nonce:              UInt64Full(rpcHeader.Nonce),
						DAAScore:           rpcHeader.DAAScore,
						BlueWork:           blueWork,
						TransactionIds:     make([]Hash, len(rpcBlock.Transactions)),
					}
					keyBlock := H2k(block.Hash)

					for i, rpcTransaction := range rpcBlock.Transactions {
						if rpcTransaction.VerboseData != nil {
							block.TransactionIds[i] = S2h(rpcTransaction.VerboseData.TransactionID)
						}
					}

					// Pruning point
					pruningPointStr := rpcHeader.PruningPoint
					pruningPointIndex, ok := PruningPointsStr[pruningPointStr]
					if !ok {
						pruningPointIndex = uint64(len(PruningPointsStr))
						hash := S2h(pruningPointStr)
						if err := dbPut(txn, PrefixPruningPoints, Serialize(&pruningPointIndex), &hash, false); err != nil {
							return err
						}
						PruningPointsStr[pruningPointStr] = pruningPointIndex
						Log(LogInf, "New pruning point %d: %s", pruningPointIndex, pruningPointStr)
					}
					block.PruningPoint = pruningPointIndex

					// Parents
					parentFound := false
					parentsStr := make(map[string]uint64, 0)
					block.ParentLevels = make([][]uint64, 0)
					block.Parents = make([]Hash, 0)
					for _, rpcParentLevel := range rpcHeader.Parents {
						rpcParent := rpcParentLevel.ParentHashes
						parentLevels2 := make([]uint64, 0)
						for _, rpcParent := range rpcParent {
							parentIndex, ok := parentsStr[rpcParent]
							if !ok {
								parentHash := S2h(rpcParent)
								block.Parents = append(block.Parents, parentHash)
								if err := dbPut(txn, PrefixBlockChild, append(parentHash[:KeyLength], keyBlock[:]...), nil, false); err != nil {
									return err
								}
								parentIndex = uint64(len(parentsStr))
								parentsStr[rpcParent] = parentIndex
								if rpcParent == rpcVData.SelectedParentHash {
									block.SelectedParent = parentIndex
									parentFound = true
								}
							}
							parentLevels2 = append(parentLevels2, parentIndex)
						}
						block.ParentLevels = append(block.ParentLevels, parentLevels2)
					}
					if !parentFound {
						Log(LogInf, "Selected parent not found (genesis?): %s", rpcVData.SelectedParentHash)
						block.Parents = append(block.Parents, S2h(rpcVData.SelectedParentHash))
					}

					if err := dbPut(txn, PrefixBlock, keyBlock[:], &block, false); err != nil {
						return err
					}

					keyBlueScoreBlock := make([]byte, 8)
					binary.BigEndian.PutUint64(keyBlueScoreBlock, block.BlueScore)
					keyBlueScoreBlock = append(keyBlueScoreBlock, keyBlock[:]...)
					if err := dbPut(txn, PrefixBlueScoreBlock, keyBlueScoreBlock, nil, false); err != nil {
						return err
					}

					// Transactions
					for _, rpcTransaction := range rpcBlock.Transactions {
						rpcTxVData := rpcTransaction.VerboseData
						if rpcTxVData == nil {
							continue
						}
						var extraData []byte
						if len(rpcTransaction.Payload) >= 120 {
							extraData, _ = hex.DecodeString(rpcTransaction.Payload[120:])
						}
						rpcInputs := rpcTransaction.Inputs
						rpcOutputs := rpcTransaction.Outputs
						transaction := Transaction{
							Hash:      S2h(rpcTxVData.Hash),
							Id:        S2h(rpcTxVData.TransactionID),
							Version:   rpcTransaction.Version,
							LockTime:  rpcTransaction.LockTime,
							Payload:   S2b(rpcTransaction.Payload),
							ExtraData: string(extraData),
							Mass:      rpcTxVData.Mass,
							Inputs:    make([]Input, len(rpcInputs)),
							Outputs:   make([]Output, len(rpcOutputs)),
						}
						key := H2k(transaction.Id)

						for i, rpcInput := range rpcInputs {
							rpcPrevious := rpcInput.PreviousOutpoint
							previousTransactionId := S2h(rpcPrevious.TransactionID)
							transaction.Inputs[i] = Input{
								Sequence:              rpcInput.Sequence,
								PreviousTransactionID: previousTransactionId,
								PreviousIndex:         rpcPrevious.Index,
								SignatureScript:       S2b(rpcInput.SignatureScript),
								SignatureOpCount:      rpcInput.SigOpCount,
							}
							if err := dbPut(txn, PrefixTransactionSpent, Serialize(&TransactionSpent{H2k(previousTransactionId), key}), nil, false); err != nil {
								return err
							}
						}
						for i, rpcOutput := range rpcOutputs {
							rpcSPK := rpcOutput.ScriptPublicKey
							rpcOutputVData := rpcOutput.VerboseData
							address := rpcOutputVData.ScriptPublicKeyAddress
							transaction.Outputs[i] = Output{
								Amount:                 rpcOutput.Amount,
								ScriptPublicKeyVersion: rpcSPK.Version,
								ScriptPublicKey:        S2b(rpcSPK.Script),
								ScriptPublicKeyType:    rpcOutputVData.ScriptPublicKeyType,
								ScriptPublicKeyAddress: address,
							}
							if address != "" {
								if err := dbPut(txn, PrefixAddress, Serialize(&AddressTransaction{address, key}), nil, false); err != nil {
									return err
								}
							}
						}

						if err := dbPut(txn, PrefixTransaction, key[:], &transaction, false); err != nil {
							return err
						}

						if err := dbPut(txn, PrefixTransactionHash, transaction.Hash[:KeyLength], &key, false); err != nil {
							return err
						}

						if err := dbPut(txn, PrefixTransactionBlock, Serialize(&TransactionBlock{key, keyBlock}), nil, false); err != nil {
							return err
						}

					}
					LatestHashesTop = (LatestHashesTop + 1) % len(LatestHashes)
					LatestHashes[LatestHashesTop] = &blockHash
				}
				bluestHash := writeElem.BluestHash
				if bluestHash != nil {
					Log(LogDbg, "Writing the bluest block: %s", hex.EncodeToString((*bluestHash)[:]))
					if err := dbPut(txn, PrefixBluestBlock, nil, bluestHash, true); err != nil {
						return err
					}
				}
				rpcBlockDagInfo := writeElem.RpcBlockDagInfo
				blockDAGInfo := BlockDAGInfo{
					KaspadVersion:       KaspadVersion,
					BlockCount:          rpcBlockDagInfo.BlockCount,
					HeaderCount:         rpcBlockDagInfo.HeaderCount,
					Difficulty:          rpcBlockDagInfo.Difficulty,
					PastMedianTime:      uint64(rpcBlockDagInfo.PastMedianTime),
					VirtualDAAScore:     rpcBlockDagInfo.VirtualDAAScore,
					PruningPointIndex:   PruningPointsStr[rpcBlockDagInfo.PruningPointHash],
					TipHashes:           make([]Hash, len(rpcBlockDagInfo.TipHashes)),
					VirtualParentHashes: make([]Hash, len(rpcBlockDagInfo.VirtualParentHashes)),
					CirculatingSupply:   UInt64Full(writeElem.CirculatingSupply),
				}
				for i, hashStr := range rpcBlockDagInfo.TipHashes {
					blockDAGInfo.TipHashes[i] = S2h(hashStr)
				}

				for i, hashStr := range rpcBlockDagInfo.VirtualParentHashes {
					blockDAGInfo.VirtualParentHashes[i] = S2h(hashStr)
				}
				for i, hash := range LatestHashes {
					if hash != nil {
						blockDAGInfo.LatestHashes[i] = *hash
					}
				}
				blockDAGInfo.LatestHashesTop = uint64(LatestHashesTop)
				if err := dbPut(txn, PrefixBlockDagInfo, nil, &blockDAGInfo, true); err != nil {
					return err
				}
				return nil
			})
			if !lmdb.IsMapFull(err) {
				PanicIfErr(err)
				break
			}
			info, err := DbEnv.Info()
			PanicIfErr(err)
			mapSize := info.MapSize
			mapSizeBits := bits.Len64(uint64(mapSize)) - 3
			newMapSize := mapSize&(0b111<<mapSizeBits) + 1<<mapSizeBits
			Log(LogInf, "LMDB map full, resizing: %d → %d", mapSize, newMapSize)
			PanicIfErr(DbEnv.SetMapSize(newMapSize))
		}
		Log(LogDbg, "Inserted blocks: %d", len(rpcBlocks))
	}
}

func dbGet(prefix byte, key []byte, value interface{}) bool {
	var binValue Bytes
	err := DbEnv.View(func(txn *lmdb.Txn) (err error) {
		binValue, err = txn.Get(Db, append([]byte{prefix}, key...))
		return err
	})
	if lmdb.IsNotFound(err) {
		return false
	}
	PanicIfErr(err)
	SerializeValue(false, bytes.NewBuffer(binValue), reflect.ValueOf(value).Elem())
	return true
}

//func dbGetRange(prefix byte, key []byte, value interface{}) bool {
//	_ = DbEnv.View(func(txn *lmdb.Txn) (err error) {
//		cursor, err := txn.OpenCursor(Db)
//		PanicIfErr(err)
//		key, val, err := cursor.Get([]byte{PrefixPruningPoints}, nil, lmdb.SetRange)
//		if lmdb.IsErrno(err, lmdb.NotFound) {
//			return err
//		}
//		PanicIfErr(err)
//		for {
//			var pruningPointIndex uint64
//			var pruningPoint Hash
//			SerializeValue(false, bytes.NewBuffer(key[1:]), reflect.ValueOf(&pruningPointIndex).Elem())
//			SerializeValue(false, bytes.NewBuffer(val), reflect.ValueOf(&pruningPoint).Elem())
//			pruningPointStr := H2s(pruningPoint)
//			PruningPointsStr[pruningPointStr] = pruningPointIndex
//			Log(LogInf, "Stored pruning point %d: %s", pruningPointIndex, pruningPointStr)
//			key, val, err = cursor.Get(nil, nil, lmdb.Next)
//			if lmdb.IsErrno(err, lmdb.NotFound) {
//				break
//			}
//			PanicIfErr(err)
//		}
//		return nil
//	})
//
//}

func dbPut(txn *lmdb.Txn, prefix byte, key []byte, value interface{}, overwrite bool) error {
	binKey := append([]byte{prefix}, key...)
	var binValue []byte
	if value != nil {
		binValue = Serialize(value)
	}
	stringKey := hex.EncodeToString(binKey)
	Log(LogTrc, "Put: %s: %s", stringKey, hex.EncodeToString(binValue))
	flags := uint(lmdb.NoOverwrite)
	if overwrite {
		flags = 0
	}
	err := txn.Put(Db, binKey, binValue, flags)
	if lmdb.IsErrno(err, lmdb.KeyExist) {
		Log(LogTrc, "dbPut key exist: %s", stringKey)
		err = nil
	}
	return err
}

func S2h(s string) (h Hash) {
	copy(h[:], S2b(s))
	return h
}

func H2k(h Hash) (k Key) {
	copy(k[:], h[:KeyLength])
	return k
}

func S2b(s string) Bytes {
	if len(s)%2 == 1 {
		s = "0" + s
	}
	b, err := hex.DecodeString(s)
	PanicIfErr(err)
	return b
}

func B2s(b Bytes) string {
	return hex.EncodeToString(b)
}

func H2s(h Hash) string {
	return B2s(h[:])
}

func FormatTimestamp(timestamp uint64) string {
	t := int64(timestamp)
	return time.Unix(t/1000, t%1000).UTC().Format("2006-01-02 15:04:05.000000000")
}

func FormatKaspa(sompi uint64) string {
	return FormatFloat(float64(sompi) / constants.SompiPerKaspa)
}

func FormatFloat(num float64) string {
	result := fmt.Sprintf("%.8f", num)
	point := strings.Index(result, ".")
	result2 := result[point:]
	i := point - 3
	for ; i > 0; i -= 3 {
		result2 = "," + result[i:i+3] + result2
	}
	result2 = result[:i+3] + result2
	return result2
}

func FormatNumber(num interface{}) string {
	return fmt.Sprintf("%d", num)
}

func PanicIfErr(err error) {
	if err != nil {
		panic(err)
	}
}
