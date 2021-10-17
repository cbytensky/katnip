package main

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/bmatsuo/lmdb-go/lmdb"
	"github.com/kaspanet/kaspad/app/appmessage"
	"github.com/kaspanet/kaspad/infrastructure/network/rpcclient"
	"os"
	"reflect"
)

const ProgName = "katnip"
const HashLength = 32

//const KeyLength = 16

type Hash [HashLength]byte
type Bytes []byte

/*type Key [KeyLength]byte
type HashRest [HashLength - KeyLength]byte*/

const (
	PrefixMaxBlueWork byte = iota
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
	Nonce              uint64
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

var logLevel *int

func main() {
	rpcServer := flag.String("rpcserver", "0.0.0.0:16110", "Kaspa RPC server address")
	logLevel = flag.Int("loglevel", LogWrn, "Log level (off = 0, error = 1, wargning = 2, info = 3, debug = 4)")
	flag.Parse()
	if len(flag.Args()) > 0 {
		flag.Usage()
		os.Exit(1)
	}

	dbEnv, err := lmdb.NewEnv()
	PanicIfErr(err)
	PanicIfErr(dbEnv.SetMapSize(1 << 30))
	cacheDir, err := os.UserCacheDir()
	PanicIfErr(err)
	dbDir := cacheDir + "/" + ProgName
	log(LogInf, "Database dir: %s", dbDir)
	PanicIfErr(os.MkdirAll(dbDir, 0755))
	PanicIfErr(dbEnv.Open(dbDir, lmdb.WriteMap|lmdb.NoLock, 0644))
	var db lmdb.DBI
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
		break
	}
	log(LogInf, "IBD finished, blocks: %d", ibdCount)
}

func insert(rpcBlocks []*appmessage.RPCBlock) {
	log(LogInf, "Inserting blocks: %d", len(rpcBlocks))
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

		// Parents
		block.Parents = make([][]Hash, len(rpcHeader.Parents))
		selectedParent := uint64(0)
		for i, rpcParentLevel := range rpcHeader.Parents {
			rpcParent := rpcParentLevel.ParentHashes
			parents := make([]Hash, len(rpcParent))
			for j, rpcParent := range rpcParent {
				parents[j] = S2h(rpcParent)
				if rpcParent == rpcVData.SelectedParentHash {
					block.SelectedParent = selectedParent
				}
				selectedParent += 1
			}
			block.Parents[i] = parents
		}

		binBuff := bytes.Buffer{}
		encoder := gob.NewEncoder(&binBuff)
		PanicIfErr(encoder.Encode(block))
		fmt.Printf("Gob Bytes: %x\n", binBuff.Bytes())
		fmt.Printf("Block before: %v\n", block)
		decoder := gob.NewDecoder(&binBuff)
		var block2 Block
		PanicIfErr(decoder.Decode(&block2))
		fmt.Printf("Block2 after: %v\n", block2)

		binBuff.Reset()
		SerializeValue(&binBuff, block)
		buffbytes := binBuff.Bytes()
		fmt.Printf("My Bytes: %x\n", buffbytes)
		block3 := &Block{}
		fmt.Printf("Block3 empty: %v\n", block3)
		metaValue := reflect.ValueOf(block3).Elem()
		DeserializeValue(&binBuff, metaValue)
		fmt.Printf("Block3 after: %v\n", block3)
		break

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
				fmt.Printf("SPKA: %s", rpcOutputVData.ScriptPublicKeyAddress)
				transaction.Outputs[i] = Output{
					Amount:                 rpcOutput.Amount,
					ScriptPublicKeyVersion: rpcSPK.Version,
					ScriptPublicKey:        S2b(rpcSPK.Script),
					ScriptPublicKeyType:    rpcOutputVData.ScriptPublicKeyType,
					ScriptPublicKeyAddress: rpcOutputVData.ScriptPublicKeyAddress,
				}
			}
		}
	}
}

func SerializeValue(buffer *bytes.Buffer, value interface{}) {
	metaValue := reflect.ValueOf(value)
	switch metaValue.Kind() {
	case reflect.Struct:
		for i := 0; i < metaValue.NumField(); i++ {
			SerializeValue(buffer, metaValue.Field(i).Interface())
		}
	case reflect.Slice:
		SerializeUint64(buffer, uint64(metaValue.Len()))
		SerializeArray(buffer, metaValue)
	case reflect.String:
		SerializeUint64(buffer, uint64(metaValue.Len()))
		buffer.WriteString(value.(string))
	case reflect.Array:
		SerializeArray(buffer, metaValue)
	case reflect.Uint16:
		SerializeUint64(buffer, uint64(value.(uint16)))
	case reflect.Uint32:
		SerializeUint64(buffer, uint64(value.(uint32)))
	case reflect.Uint64:
		SerializeUint64(buffer, value.(uint64))
	case reflect.Uint8:
		buffer.WriteByte(value.(byte))
	case reflect.Bool:
		boolValue := byte(0)
		if value.(bool) {
			boolValue = 1
		}
		buffer.WriteByte(boolValue)
	}
}

func SerializeArray(buffer *bytes.Buffer, metaValue reflect.Value) {
	for i := 0; i < metaValue.Len(); i++ {
		SerializeValue(buffer, metaValue.Index(i).Interface())
	}
}

func SerializeUint64(buffer *bytes.Buffer, value uint64) {
	valueBytes := make([]byte, 8)
	n := binary.PutUvarint(valueBytes, value)
	buffer.Write(valueBytes[:n])
}

func DeserializeValue(buffer *bytes.Buffer, metaValue reflect.Value) {
	switch metaValue.Kind() {
	case reflect.Struct:
		for i := 0; i < metaValue.NumField(); i++ {
			DeserializeValue(buffer, metaValue.Field(i))
		}
	case reflect.Slice:
		len := uint64(0)
		DeserializeUint64(buffer, reflect.ValueOf(&len).Elem())
		ilen := int(len)
		metaSlice := reflect.MakeSlice(metaValue.Type(), ilen, ilen)
		DeserializeArray(buffer, metaSlice)
		metaValue.Set(metaSlice)
	case reflect.String:
		len := uint64(0)
		DeserializeUint64(buffer,  reflect.ValueOf(&len).Elem())
		ilen := int(len)
		buf := make([]byte, ilen)
		buffer.Read(buf)
		metaValue.SetString(string(buf))
	case reflect.Array:
		DeserializeArray(buffer, metaValue)
	case reflect.Uint16, reflect.Uint32, reflect.Uint64:
		DeserializeUint64(buffer, metaValue)
	case reflect.Uint8:
		val, err := buffer.ReadByte()
		PanicIfErr(err)
		metaValue.SetUint(uint64(val))
	case reflect.Bool:
		val, err := buffer.ReadByte()
		PanicIfErr(err)
		metaValue.SetBool(val == 1)
	}
}

func DeserializeArray(buffer *bytes.Buffer, metaValue reflect.Value) {
	for i := 0; i < metaValue.Len(); i++ {
		DeserializeValue(buffer, metaValue.Index(i))
	}
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

/*func H2k(h Hash) (k Key) {
	copy(k[:], h[:KeyLength-1])
	return k
}

func H2r(h Hash) (r HashRest) {
	copy(r[:], h[KeyLength:])
	return r
}*/

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
