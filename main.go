package main

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/bmatsuo/lmdb-go/lmdb"
	"github.com/kaspanet/kaspad/app/appmessage"
	"github.com/kaspanet/kaspad/infrastructure/network/rpcclient"
	"os"
)

const ProgName = "katnip"
const HashLength = 32
const KeyLength = 16

type Hash [HashLength]byte
type Bytes []byte
type Key [KeyLength]byte
type HashRest [HashLength - KeyLength]byte

type Block struct {
	Hash               Hash
	IsHeaderOnly       bool
	BlueScore          uint64
	Version            uint32
	SelectedParent     uint
	Parents            [][]Hash
	MerkleRoot         Hash
	AcceptedMerkleRoot Hash
	UtxoCommitment     Hash
	Timestamp          int64
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
			Timestamp:          rpcHeader.Timestamp,
			Bits:               rpcHeader.Bits,
			Nonce:              rpcHeader.Nonce,
			DaaScore:           rpcHeader.DAAScore,
			BlueWork:           S2b(rpcHeader.BlueWork),
			PruningPoint:       S2h(rpcHeader.PruningPoint),
		}

		// Parents
		block.Parents = make([][]Hash, len(rpcHeader.Parents))
		selectedParent := uint(0)
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



		// Transactions
		for _, rpcTransaction := range rpcBlock.Transactions {
			rpcTxVData := rpcTransaction.VerboseData
			rpcInputs := rpcTransaction.Inputs
			rpcOutputs := rpcTransaction.Outputs
			transaction := Transaction{
				Block:    blockHash,
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
	=    buf := new(bytes.Buffer)
	serialize(buf, rpcBlocks[0].Header, 10)
	serialize(buf, rpcBlocks[0].VerboseData, -1)
	fmt.Printf("%x", buf.Bytes())
}

func S2h(s string) (h Hash) {
	copy(h[:], S2b(s))
	return h
}

func H2k(h Hash) (k Key) {
	copy(k[:], h[:KeyLength-1])
	return k
}

func H2r(h Hash) (r HashRest) {
	copy(r[:], h[KeyLength:])
	return r
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
		fmt.Fprintf(os.Stderr, LevelStr[level]+" "+format+"\n", args...)
	}
}
