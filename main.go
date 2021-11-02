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
)

const ProgName = "katnip"
const HashLength = 32

const KeyLength = 16

type Hash [HashLength]byte
type Key [KeyLength]byte
type Bytes []byte

/*
type HashRest [HashLength - KeyLength]byte*/

const (
	PrefixMaxBlueWork byte = iota
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
	Address string
	BlockKey Key
}

var logLevel *int
var (
	dbEnv *lmdb.Env
	db    lmdb.DBI
)

func main() {
	rpcServer := flag.String("rpcserver", "0.0.0.0:16110", "Kaspa RPC server address")
	logLevel = flag.Int("loglevel", LogWrn, "Log level (off = 0, error = 1, wargning = 2, info = 3, debug = 4)")
	flag.Parse()
	if len(flag.Args()) > 0 {
		flag.Usage()
		os.Exit(1)
	}

	var err error
	dbEnv, err = lmdb.NewEnv()
	PanicIfErr(err)
	PanicIfErr(dbEnv.SetMapSize(1 << 30))
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

	MaxBlueWorkKey := []byte{PrefixMaxBlueWork}

	var bluestHash []byte
	err = dbEnv.View(func(txn *lmdb.Txn) (err error) {
		bluestHash, err = txn.Get(db, MaxBlueWorkKey)
		return err
	})
	lowHashStr := ""
	if !lmdb.IsNotFound(err) {
		PanicIfErr(err)
		lowHashStr = hex.EncodeToString(bluestHash)
		log(LogInf, "Bluest block hash: "+lowHashStr)
	}

	log(LogInf, "Connecting to Kaspad: "+*rpcServer)
	rpcClient, err := rpcclient.NewRPCClient(*rpcServer)
	PanicIfErr(err)
	log(LogInf, "IBD stared")
	var response *appmessage.GetBlocksResponseMessage
	ibdCount := 0
	for {
		for {
			log(LogDbg, "getBlocks: "+lowHashStr)
			response, err = rpcClient.GetBlocks(lowHashStr, true, true)
			if err == nil {
				break
			}
			log(LogErr, "getBlocks: %s", err)
		}
		rpcBlocks := response.Blocks
		count := len(rpcBlocks)
		if lowHashStr != "" && count == 1 {
			break
		}
		ibdCount += count
		lowHashStr = rpcBlocks[count-1].VerboseData.Hash
		insert(rpcBlocks)
	}
	log(LogInf, "IBD finished, blocks: %d", ibdCount)
}

func insert(rpcBlocks []*appmessage.RPCBlock) {
	log(LogInf, "Inserting blocks: %d", len(rpcBlocks))
	for {
		err := dbEnv.Update(func(txn *lmdb.Txn) (err error) {
			for _, rpcBlock := range rpcBlocks {
				rpcVData := rpcBlock.VerboseData
				rpcHeader := rpcBlock.Header
				blockHash := S2h(rpcVData.Hash)
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
					BlueWork:           S2b(rpcHeader.BlueWork),
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
						if err := dbPut(txn, PrefixBlockChild, parent[:KeyLength-1], keyBlock); err != nil {
							return err
						}
					}
					block.Parents[i] = parents
				}

				if err := dbPut(txn, PrefixBlock, keyBlock[:], &block); err != nil {
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
						fmt.Printf("SPKA: %s\n", rpcOutputVData.ScriptPublicKeyAddress)
						address := rpcOutputVData.ScriptPublicKeyAddress
						transaction.Outputs[i] = Output{
							Amount:                 rpcOutput.Amount,
							ScriptPublicKeyVersion: rpcSPK.Version,
							ScriptPublicKey:        S2b(rpcSPK.Script),
							ScriptPublicKeyType:    rpcOutputVData.ScriptPublicKeyType,
							ScriptPublicKeyAddress: address,
						}
						if address != "" {
							if err := dbPut(txn, PrefixAddress, Serialize(&AddressKey{address, keyBlock}), []byte{}); err != nil {
								return err
							}
						}
					}

					key := H2k(transaction.Id)
					if err := dbPut(txn, PrefixTransaction, key[:], &transaction); err != nil {
						return err
					}

					if err := dbPut(txn, PrefixTransactionHash, transaction.Id[:KeyLength-1], &key); err != nil {
						return err
					}

					if err := dbPut(txn, PrefixTransactionBlock, append(key[:], keyBlock[:]...), &[]byte{}); err != nil {
						return err
					}

				}
				break
			}
			return nil
		})
		if err != lmdb.MapFull {
			PanicIfErr(err)
			break
		}
		info, err := dbEnv.Info()
		PanicIfErr(err)
		dbEnv.SetMapSize(info.MapSize + 1<<(bits.Len64(uint64(info.MapSize))-3))
	}
}

func dbPut(txn *lmdb.Txn, prefix byte, key []byte, value interface{}) error {
	err := txn.Put(db, append([]byte{prefix}, key...), Serialize(value), lmdb.NoOverwrite)
	if err != nil && err.(*lmdb.OpError).Errno == lmdb.KeyExist {
		log(LogDbg, "dbPut key exist: %v", key)
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
)

var LevelStr = [...]string{"", "ERR", "WRN", "INF", "DBG"}

func log(level byte, format string, args ...interface{}) {
	if level <= byte(*logLevel) {
		_, _ = fmt.Fprintf(os.Stderr, LevelStr[level]+" "+format+"\n", args...)
	}
}
