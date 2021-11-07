package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/bmatsuo/lmdb-go/lmdb"
	"github.com/kaspanet/kaspad/app/appmessage"
	"github.com/kaspanet/kaspad/infrastructure/network/rpcclient"
	"math/bits"
	"net/http"
	"os"
	"reflect"
	"strings"
)

const KeyLength = 16

const (
	PrefixBluestBlock byte = iota
	PrefixBlock
	PrefixBlockChild
	PrefixTransaction
	PrefixTransactionHash
	PrefixTransactionBlock
	PrefixAddress
)

const (
	LogOff = iota
	LogErr
	LogWrn
	LogInf
	LogDbg
	LogTrc
)

type (
	UInt64Full uint64
	Hash       [32]byte
	Key        [KeyLength]byte
	Bytes      []byte
	RpcBlocks  []*appmessage.RPCBlock

	AddressKey struct {
		Address  string
		BlockKey Key
	}

	WriteChanElem struct {
		RpcBlocks  RpcBlocks
		BluestHash *Hash
	}
)

type (
	Block struct {
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
		Nonce              UInt64Full
		DaaScore           uint64
		BlueWork           Bytes
		PruningPoint       Hash
	}

	Transaction struct {
		Hash     Hash
		Id       Hash
		Version  uint16
		LockTime uint64
		Payload  Bytes
		Mass     uint64
		Inputs   []Input
		Outputs  []Output
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
)

var logLevel *int
var LevelStr = [...]string{"", "ERR", "WRN", "INF", "DBG", "TRC"}

var (
	dbEnv *lmdb.Env
	db    lmdb.DBI
)

var WriteChan = make(chan WriteChanElem, 100)
var MaxBlueWorkStr string
var BluestHashStr string

func main() {
	rpcServerAddr := flag.String("rpcserver", "0.0.0.0:16110", "Kaspa RPC server address")
	httpServerAddr := flag.String("httpserver", "0.0.0.0:8080", "HTTP server address and port")
	logLevel = flag.Int("loglevel", LogWrn, "Log level (off = 0, error = 1, warning = 2, info = 3, debug = 4, trace = 5), default: 2")
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
	dbDir := cacheDir + "/katnip"
	Log(LogInf, "Database dir: %s", dbDir)
	PanicIfErr(os.MkdirAll(dbDir, 0755))
	PanicIfErr(dbEnv.Open(dbDir, lmdb.WriteMap|lmdb.NoLock, 0644))
	PanicIfErr(dbEnv.Update(func(txn *lmdb.Txn) (err error) {
		db, err = txn.OpenRoot(0)
		return err
	}))

	var rpcClient *rpcclient.RPCClient
	for {
		Log(LogInf, "Connecting to Kaspad: %s", *rpcServerAddr)
		rpcClient, err = rpcclient.NewRPCClient(*rpcServerAddr)
		if err == nil {
			break
		} else {
			Log(LogErr, "%v", err)
		}
	}

	PanicIfErr(err)
	Log(LogInf, "IBD stared")

	var BluestHash Hash
	if dbGet(PrefixBluestBlock, nil, &BluestHash) {
		BluestHashStr = hex.EncodeToString(BluestHash[:])
		Log(LogInf, "Bluest block hash: %s", BluestHashStr)
	}

	go insert()

	http.Handle("/style.css", http.FileServer(http.Dir(".")))
	http.HandleFunc("/block/", func(w http.ResponseWriter, r *http.Request) {
		Log(LogErr, "Path: %v", r.URL.Path)
		path := strings.Split(r.URL.Path, "/")
		if len(path) < 3 {
			HttpError(errors.New(fmt.Sprintf("Malformed path: %v", path)), "", w)
			return
		}
		hashSlice, err := hex.DecodeString(path[2])
		if err != nil {
			HttpError(errors.New(fmt.Sprintf("Bad hash: %v", path)), " decoding hash", w)
			return
		}
		block := Block{}
		//Log(LogErr, "Get Block full (%d): %v", len(hashSlice), hashSlice)
		//Log(LogErr, "Get Block (%d): %v", len(hashSlice[:KeyLength]), hashSlice[0:KeyLength])
		if !dbGet(PrefixBlock, hashSlice[0:KeyLength], &block) {
			HttpError(errors.New("key not found: "+path[2]), "", w)
			return
		}

		w.Write([]byte("<!DOCTYPE html>\n" +
			"<html xmlns=\"http://www.w3.org/1999/xhtml\" lang=\"en\">\n" +
			"<head>\n" +
			"<meta charset=\"UTF-8\"/>\n" +
			"<meta name=\"viewport\" content=\"width=device-width\"/>\n" +
			"<link rel=\"stylesheet\" href=\"/style.css\"/>\n" +
			"<title>Katnip</title>\n" +
			"</head>\n" +
			"<body>\n" +
			"<table>\n" +
			"<tbody>\n"))
		metaStruct := reflect.ValueOf(block)
		for i := 0; i < metaStruct.NumField(); i++ {
			field := metaStruct.Type().Field(i)
			v := metaStruct.Field(i).Interface()
			name := field.Name
			title := ToTitle(name)
			fmt.Fprintf(w, "<tr><th>%s</th><td>%v</td></tr>\n", title, v)
		}
		w.Write([]byte("</tbody>\n" +
			"</table>\n" +
			"</html>"))
	})
	go http.ListenAndServe(*httpServerAddr, nil)

	var response *appmessage.GetBlocksResponseMessage
	ibdCount := 0
	for {
		Log(LogInf, "getBlocks: %s", BluestHashStr)
		response, err = rpcClient.GetBlocks(BluestHashStr, true, true)
		if err != nil {
			Log(LogErr, "getBlocks: %s", err)
			continue
		}
		rpcBlocks := response.Blocks
		count := len(rpcBlocks)
		if BluestHashStr != "" && count == 1 {
			break
		}
		ibdCount += count
		AddToWriteChan(rpcBlocks)
	}
	Log(LogInf, "IBD finished, blocks: %d", ibdCount)
	PanicIfErr(rpcClient.RegisterForBlockAddedNotifications(func(notification *appmessage.BlockAddedNotificationMessage) {
		strHash := notification.Block.VerboseData.Hash
		blockResponse, err := rpcClient.GetBlock(strHash, true)
		PanicIfErr(err)
		AddToWriteChan(RpcBlocks{blockResponse.Block})
	}))

	select {}
}

func HttpError(err error, title string, w http.ResponseWriter) bool {
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("<!DOCTYPE html>\n" +
		"<html xmlns=\"http://www.w3.org/1999/xhtml\" lang=\"en\">\n" +
		"<head>\n" +
		"<meta charset=\"UTF-8\"/>\n" +
		"<meta name=\"viewport\" content=\"width=device-width\"/>\n" +
		"<link rel=\"stylesheet\" href=\"/style.css\"/>\n" +
		"<title>Katnip</title>\n" +
		"</head>\n" +
		"<body>\n" +
		"<h1>Error" + title + "</h1>\n" +
		"<p>" + fmt.Sprintf("%v", err) + "</p>\n" +
		"</body>\n" +
		"</html>"))
	return true
}

func AddToWriteChan(rpcBlocks RpcBlocks) {
	var bluestHashToWrite *Hash = nil
	for _, rpcBlock := range rpcBlocks {
		blueWorkStr := rpcBlock.Header.BlueWork
		if len(blueWorkStr) > len(MaxBlueWorkStr) || len(blueWorkStr) == len(MaxBlueWorkStr) && blueWorkStr > MaxBlueWorkStr {
			MaxBlueWorkStr = blueWorkStr
			BluestHashStr = rpcBlock.VerboseData.Hash
			bluestHash := S2h(BluestHashStr)
			bluestHashToWrite = &bluestHash
		}
	}
	WriteChan <- WriteChanElem{rpcBlocks, bluestHashToWrite}
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
						Nonce:              UInt64Full(rpcHeader.Nonce),
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
							if err := dbPut(txn, PrefixBlockChild, parent[:KeyLength], &keyBlock, false); err != nil {
								return err
							}
						}
						block.Parents[i] = parents
					}

					//Log(LogErr, "keyBlock full (%d): %v", len(block.Hash[:]), block.Hash[:])
					//Log(LogErr, "keyBlock (%d): %v", len(keyBlock[:]), keyBlock[:])
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

						if err := dbPut(txn, PrefixTransactionHash, transaction.Id[:KeyLength], &key, false); err != nil {
							return err
						}

						if err := dbPut(txn, PrefixTransactionBlock, append(key[:], keyBlock[:]...), &[]byte{}, false); err != nil {
							return err
						}

					}
				}
				bluestHash := writeElem.BluestHash
				if bluestHash != nil {
					Log(LogInf, "Writing bluest block: %s", hex.EncodeToString((*bluestHash)[:]))
					if err := dbPut(txn, PrefixBluestBlock, nil, bluestHash, true); err != nil {
						return err
					}
				}
				return nil
			})
			if !lmdb.IsMapFull(err) {
				PanicIfErr(err)
				break
			}
			info, err := dbEnv.Info()
			PanicIfErr(err)
			mapSize := info.MapSize
			mapSizeBits := bits.Len64(uint64(mapSize)) - 3
			newMapSize := mapSize&(0b111<<mapSizeBits) + 1<<mapSizeBits
			Log(LogInf, "Map full, resizing: %d → %d", mapSize, newMapSize)
			PanicIfErr(dbEnv.SetMapSize(newMapSize))
		}
		Log(LogInf, "Inserted blocks: %d", len(rpcBlocks))
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
	Log(LogTrc, "Put: %s: %s", stringKey, hex.EncodeToString(binValue))
	flags := uint(lmdb.NoOverwrite)
	if overwrite {
		flags = 0
	}
	err := txn.Put(db, binKey, binValue, flags)
	if lmdb.IsErrno(err, lmdb.KeyExist) {
		Log(LogDbg, "dbPut key exist: %s", stringKey)
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
		for i := 0; i < metaValue.NumField(); i++ {
			metaField := metaValue.Field(i)
			if _, isNonce := value.(UInt64Full); isNonce {
				uintBytes := make([]byte, 8)
				if isSer {
					binary.LittleEndian.PutUint64(uintBytes, metaField.Interface().(uint64))
					buffer.Write(uintBytes)
				} else {
					_, err := buffer.Read(uintBytes)
					PanicIfErr(err)
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
			sliceLen := DeserializeLen(buffer)
			metaSlice := reflect.MakeSlice(metaValue.Type(), sliceLen, sliceLen)
			SerializeArray(false, buffer, metaSlice)
			metaValue.Set(metaSlice)
		}
	case reflect.String:
		if isSer {
			SerializeLen(buffer, metaValue)
			buffer.WriteString(value.(string))
		} else {
			buf := make([]byte, DeserializeLen(buffer))
			_, err := buffer.Read(buf)
			PanicIfErr(err)
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
	sliceLen := uint64(0)
	DeserializeUint64(buffer, reflect.ValueOf(&sliceLen).Elem())
	return int(sliceLen)
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

func PanicIfErr(err error) {
	if err != nil {
		panic(err)
	}
}

func Log(level byte, format string, args ...interface{}) {
	if level <= byte(*logLevel) {
		_, _ = fmt.Fprintf(os.Stderr, LevelStr[level]+" "+format+"\n", args...)
	}
}

func isUpper(c byte) bool {
	return c >= 'A' && c <= 'Z'
}

func ToTitle(s string) string {
	r := make([]byte, 0, len(s)+2)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUpper(c) && i > 0 && i+1 < len(s) && !(isUpper(s[i-1]) && isUpper(s[i+1])) {
			r = append(r, ' ')
		}
		r = append(r, c)
	}
	return string(r)
}

func TrimZeroes(s string) string {
	var i int
	for i = 0; i < len(s); i++ {
		if s[i] != '0' {
			break
		}
	}
	s = s[i:]
	return s
}
