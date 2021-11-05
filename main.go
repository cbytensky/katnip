package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/bmatsuo/lmdb-go/lmdb"
	"github.com/kaspanet/kaspad/app/appmessage"
	"github.com/kaspanet/kaspad/infrastructure/network/rpcclient"
	"math/bits"
	"os"
	"reflect"
	"time"
)

const ProgName = "katnip"
const HashLength = 32

const KeyLength = 16

type Hash [HashLength]byte
type Key [KeyLength]byte
type Bytes []byte
type RpcBlocks []*appmessage.RPCBlock
type WriteChanType chan WriteChanElem

/*
type HashRest [HashLength - KeyLength]byte*/

const (
	PrefixBluestBlock byte = iota
	PrefixBlock
	PrefixBlockChild
	PrefixTransaction
	PrefixTransactionHash
	PrefixTransactionBlock
	PrefixAddress
)

type Block struct {
	Hash               Hash
	IsHeaderOnly       bool
	BlueScore          uint64
	Version            uint32
	SelectedParent     uint64
	Parents            [][]Hash
	MerkleRoot         Hash
	AcceptedMerkleRoot Hash
	UtxoCommitment     Hash
	Timestamp          uint64
	Bits               uint32
	Nonce              uint64 "f"
	DaaScore           uint64
	BlueWork           Bytes
	PruningPoint       Hash
}

type Transaction struct {
	Hash     Hash
	Id       Hash
	Version  uint16
	LockTime uint64
	Payload  Bytes
	Mass     uint64
	Inputs   []Input
	Outputs  []Output
}

type Input struct {
	Sequence              uint64
	PreviousTransactionID Hash
	PreviousIndex         uint32
	SignatureScript       Bytes
	SignatureOpCount      byte
}

type Output struct {
	Amount                 uint64
	ScriptPublicKeyVersion uint16
	ScriptPublicKey        Bytes
	ScriptPublicKeyType    string
	ScriptPublicKeyAddress string
}

type AddressKey struct {
	Address  string
	BlockKey Key
}

type WriteChanElem struct {
	RpcBlocks  RpcBlocks
	BluestHash Hash
}

var logLevel *int
var (
	dbEnv *lmdb.Env
	db    lmdb.DBI
)

var WriteChan WriteChanType
var MaxBlueWork Bytes
var BluestHash Hash

func main() {
	rpcServer := flag.String("rpcserver", "0.0.0.0:16110", "Kaspa RPC server address")
	logLevel = flag.Int("loglevel", LogWrn, "Log level (off = 0, error = 1, wargning = 2, info = 3, debug = 4, trace = 5), default: 2")
	flag.Parse()
	if len(flag.Args()) > 0 {
		flag.Usage()
		os.Exit(1)
	}

	var err error
	dbEnv, err = lmdb.NewEnv()
	PanicIfErr(err)
	PanicIfErr(dbEnv.SetMapSize(1 << 20))
	cacheDir, err := os.UserCacheDir()
	PanicIfErr(err)
	dbDir := cacheDir + "/" + ProgName
	log(LogInf, "Database dir: %s", dbDir)
	PanicIfErr(os.MkdirAll(dbDir, 0755))
	PanicIfErr(dbEnv.Open(dbDir, lmdb.WriteMap|lmdb.NoLock, 0644))
	PanicIfErr(dbEnv.Update(func(txn *lmdb.Txn) (err error) {
		db, err = txn.OpenRoot(0)
		return err
	}))

	lowHashStr := ""
	maxBlueWork := ""
	if dbGet(PrefixBluestBlock, nil, &BluestHash) {
		lowHashStr = hex.EncodeToString(BluestHash[:])
		log(LogInf, "Bluest block hash: %s", lowHashStr)
	}

	var rpcClient *rpcclient.RPCClient
	for {
		log(LogInf, "Connecting to Kaspad: %s", *rpcServer)
		rpcClient, err = rpcclient.NewRPCClient(*rpcServer)
		if err == nil {
			break
		} else {
			log(LogErr, "%v", err)
		}
	}

	PanicIfErr(err)
	log(LogInf, "IBD stared")

	WriteChan = make(WriteChanType, 10)
	go insert()

	var response *appmessage.GetBlocksResponseMessage
	ibdCount := 0
	for {
		log(LogInf, "getBlocks: %s", lowHashStr)
		response, err = rpcClient.GetBlocks(lowHashStr, true, true)
		if err != nil {
			log(LogErr, "getBlocks: %s", err)
			continue
		}
		rpcBlocks := response.Blocks
		count := len(rpcBlocks)
		if lowHashStr != "" && count == 1 {
			break
		}
		ibdCount += count

		for _, rpcBlock := range rpcBlocks {
			blueWork := rpcBlock.Header.BlueWork
			if len(blueWork) > len(maxBlueWork) || len(blueWork) == len(maxBlueWork) && blueWork > maxBlueWork {
				maxBlueWork = blueWork
				lowHashStr = rpcBlock.VerboseData.Hash
			}
		}
		WriteChan <- WriteChanElem{rpcBlocks, S2h(lowHashStr)}
	}
	log(LogInf, "IBD finished, blocks: %d", ibdCount)
	for {
		blockAdded := <-rpcClient.OnBlockAdded:
			blockResponse, err := client.GetBlock(blockAdded.Block.VerboseData.Hash, true)
			PanicIfErr(err)
			insert([]*appmessage.RPCBlock{blockResponse.Block})
	}
}

func insert() {
	for {
		writeElem := <-WriteChan
		rpcBlocks := writeElem.RpcBlocks
		for {
			err := dbEnv.Update(func(txn *lmdb.Txn) (err error) {
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
						UtxoCommitment:     S2h(rpcHeader.UTXOCommitment),
						Timestamp:          uint64(rpcHeader.Timestamp),
						Bits:               rpcHeader.Bits,
						Nonce:              rpcHeader.Nonce,
						DaaScore:           rpcHeader.DAAScore,
						BlueWork:           blueWork,
						PruningPoint:       S2h(rpcHeader.PruningPoint),
					}

					keyBlock := H2k(block.Hash)

					// Parents
					block.Parents = make([][]Hash, len(rpcHeader.Parents))
					selectedParent := uint64(0)
					for i, rpcParentLevel := range rpcHeader.Parents {
						rpcParent := rpcParentLevel.ParentHashes
						parents := make([]Hash, len(rpcParent))
						for j, rpcParent := range rpcParent {
							parent := S2h(rpcParent)
							parents[j] = parent
							if rpcParent == rpcVData.SelectedParentHash {
								block.SelectedParent = selectedParent
							}
							selectedParent += 1
							if err := dbPut(txn, PrefixBlockChild, parent[:KeyLength-1], &keyBlock, false); err != nil {
								return err
							}
						}
						block.Parents[i] = parents
					}

					if err := dbPut(txn, PrefixBlock, keyBlock[:], &block, false); err != nil {
						return err
					}

					// Transactions
					for _, rpcTransaction := range rpcBlock.Transactions {
						rpcTxVData := rpcTransaction.VerboseData
						rpcInputs := rpcTransaction.Inputs
						rpcOutputs := rpcTransaction.Outputs
						transaction := Transaction{
							Hash:     S2h(rpcTxVData.Hash),
							Id:       S2h(rpcTxVData.TransactionID),
							Version:  rpcTransaction.Version,
							LockTime: rpcTransaction.LockTime,
							Payload:  S2b(rpcTransaction.Payload),
							Mass:     rpcTxVData.Mass,
							Inputs:   make([]Input, len(rpcInputs)),
							Outputs:  make([]Output, len(rpcOutputs)),
						}

						for i, rpcInput := range rpcInputs {
							rpcPrevious := rpcInput.PreviousOutpoint
							transaction.Inputs[i] = Input{
								Sequence:              rpcInput.Sequence,
								PreviousTransactionID: S2h(rpcPrevious.TransactionID),
								PreviousIndex:         rpcPrevious.Index,
								SignatureScript:       S2b(rpcInput.SignatureScript),
								SignatureOpCount:      rpcInput.SigOpCount,
							}
						}
						for i, rpcOutput := range rpcOutputs {
							rpcSPK := rpcOutput.ScriptPublicKey
							rpcOutputVData := rpcOutput.VerboseData
							log(LogDbg, "SPKA: %s", rpcOutputVData.ScriptPublicKeyAddress)
							address := rpcOutputVData.ScriptPublicKeyAddress
							transaction.Outputs[i] = Output{
								Amount:                 rpcOutput.Amount,
								ScriptPublicKeyVersion: rpcSPK.Version,
								ScriptPublicKey:        S2b(rpcSPK.Script),
								ScriptPublicKeyType:    rpcOutputVData.ScriptPublicKeyType,
								ScriptPublicKeyAddress: address,
							}
							if address != "" {
								if err := dbPut(txn, PrefixAddress, Serialize(&AddressKey{address, keyBlock}), &[]byte{}, false); err != nil {
									return err
								}
							}
						}

						key := H2k(transaction.Id)
						if err := dbPut(txn, PrefixTransaction, key[:], &transaction, false); err != nil {
							return err
						}

						if err := dbPut(txn, PrefixTransactionHash, transaction.Id[:KeyLength-1], &key, false); err != nil {
							return err
						}

						if err := dbPut(txn, PrefixTransactionBlock, append(key[:], keyBlock[:]...), &[]byte{}, false); err != nil {
							return err
						}

					}
				}
				log(LogInf, "Writing bluest block: %s", hex.EncodeToString(writeElem.BluestHash[:]))
				return dbPut(txn, PrefixBluestBlock, nil, &writeElem.BluestHash, true)
			})
			if !lmdb.IsMapFull(err) {
				PanicIfErr(err)
				break
			}
			info, err := dbEnv.Info()
			PanicIfErr(err)
			mapSize := info.MapSize
			newMapSize := mapSize + 1<<(bits.Len64(uint64(mapSize))-3)
			log(LogInf, "Map full, resizing: %d → %d", mapSize, newMapSize)
			PanicIfErr(dbEnv.SetMapSize(newMapSize))
		}
		log(LogInf, "Inserted blocks: %d", len(rpcBlocks))
	}
}

func dbGet(prefix byte, key []byte, value interface{}) bool {
	var binValue Bytes
	err := dbEnv.View(func(txn *lmdb.Txn) (err error) {
		binValue, err = txn.Get(db, append([]byte{prefix}, key...))
		return err
	})
	if lmdb.IsNotFound(err) {
		return false
	}
	PanicIfErr(err)
	SerializeValue(false, bytes.NewBuffer(binValue), reflect.ValueOf(value).Elem())
	return true
}

func dbPut(txn *lmdb.Txn, prefix byte, key []byte, value interface{}, overwrite bool) error {
	binKey := append([]byte{prefix}, key...)
	binValue := Serialize(value)
	stringKey := hex.EncodeToString(binKey)
	log(LogTrc, "Put: %s: %s", stringKey, hex.EncodeToString(binValue))
	flags := uint(lmdb.NoOverwrite)
	if overwrite {
		flags = 0
	}
	err := txn.Put(db, binKey, binValue, flags)
	if lmdb.IsErrno(err, lmdb.KeyExist) {
		log(LogDbg, "dbPut key exist: %s", stringKey)
		err = nil
	}
	return err
}

func Serialize(value interface{}) []byte {
	buff := bytes.Buffer{}
	SerializeValue(true, &buff, reflect.ValueOf(value).Elem())
	return buff.Bytes()
}

func SerializeValue(isSer bool, buffer *bytes.Buffer, metaValue reflect.Value) {
	value := metaValue.Interface()
	switch metaValue.Kind() {
	case reflect.Struct:
		structType := metaValue.Type()
		for i := 0; i < metaValue.NumField(); i++ {
			metaField := metaValue.Field(i)
			if string(structType.Field(i).Tag) == "f" {
				uintBytes := make([]byte, 8)
				if isSer {
					binary.LittleEndian.PutUint64(uintBytes, metaField.Interface().(uint64))
					buffer.Write(uintBytes)
				} else {
					buffer.Read(uintBytes)
					metaField.SetUint(binary.LittleEndian.Uint64(uintBytes))
				}
			} else {
				SerializeValue(isSer, buffer, metaField)
			}
		}
	case reflect.Slice:
		if isSer {
			SerializeLen(buffer, metaValue)
			SerializeArray(isSer, buffer, metaValue)
		} else {
			len := DeserializeLen(buffer)
			metaSlice := reflect.MakeSlice(metaValue.Type(), len, len)
			SerializeArray(false, buffer, metaSlice)
			metaValue.Set(metaSlice)
		}
	case reflect.String:
		if isSer {
			SerializeLen(buffer, metaValue)
			buffer.WriteString(value.(string))
		} else {
			buf := make([]byte, DeserializeLen(buffer))
			buffer.Read(buf)
			metaValue.SetString(string(buf))
		}
	case reflect.Array:
		SerializeArray(isSer, buffer, metaValue)
	case reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if isSer {
			var u64value uint64
			switch value.(type) {
			case uint16:
				u64value = uint64(value.(uint16))
			case uint32:
				u64value = uint64(value.(uint32))
			case uint64:
				u64value = value.(uint64)
			}
			SerializeUint64(buffer, u64value)
		} else {
			DeserializeUint64(buffer, metaValue)
		}
	case reflect.Uint8:
		if isSer {
			buffer.WriteByte(value.(byte))
		} else {
			metaValue.SetUint(uint64(BufferReadByte(buffer)))
		}
	case reflect.Bool:
		if isSer {
			boolValue := byte(0)
			if value.(bool) {
				boolValue = 1
			}
			buffer.WriteByte(boolValue)
		} else {
			metaValue.SetBool(BufferReadByte(buffer) == 1)
		}
	}
}

func BufferReadByte(buffer *bytes.Buffer) byte {
	val, err := buffer.ReadByte()
	PanicIfErr(err)
	return val
}

func SerializeLen(buffer *bytes.Buffer, metaValue reflect.Value) {
	SerializeUint64(buffer, uint64(metaValue.Len()))
}

func DeserializeLen(buffer *bytes.Buffer) int {
	len := uint64(0)
	DeserializeUint64(buffer, reflect.ValueOf(&len).Elem())
	return int(len)
}

func SerializeArray(isSer bool, buffer *bytes.Buffer, metaValue reflect.Value) {
	for i := 0; i < metaValue.Len(); i++ {
		SerializeValue(isSer, buffer, metaValue.Index(i))
	}
}

func SerializeUint64(buffer *bytes.Buffer, value uint64) {
	valueBytes := make([]byte, 8)
	n := binary.PutUvarint(valueBytes, value)
	buffer.Write(valueBytes[:n])
}

func DeserializeUint64(buffer *bytes.Buffer, metaValue reflect.Value) {
	value, err := binary.ReadUvarint(buffer)
	PanicIfErr(err)
	metaValue.SetUint(value)
}

func S2h(s string) (h Hash) {
	copy(h[:], S2b(s))
	return h
}

func H2k(h Hash) (k Key) {
	copy(k[:], h[:KeyLength-1])
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

func PanicIfErr(err error) {
	if err != nil {
		panic(err)
	}
}

const (
	LogOff = iota
	LogErr
	LogWrn
	LogInf
	LogDbg
	LogTrc
)

var LevelStr = [...]string{"", "ERR", "WRN", "INF", "DBG", "TRC"}

func log(level byte, format string, args ...interface{}) {
	if level <= byte(*logLevel) {
		_, _ = fmt.Fprintf(os.Stderr, LevelStr[level]+" "+format+"\n", args...)
	}
}
