package xpaxos

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/gob"
	"encoding/json"
	"labrpc"
	"log"
	"sync"
	"time"
)

const BITSIZE = 1024 // RSA private key bit size

const (
	REPLICATE = iota
	PREPARE   = iota
	COMMIT    = iota
	REPLY     = iota
)

type XPaxos struct {
	mu               sync.Mutex
	persister        *Persister
	replicas         []*labrpc.ClientEnd
	synchronousGroup map[int]bool
	id               int
	view             int
	prepareSeqNum    int
	executeSeqNum    int
	prepareLog       []PrepareLogEntry
	commitLog        []CommitLogEntry
	privateKey       *rsa.PrivateKey
	publicKeys       map[int]*rsa.PublicKey
}

type Message struct {
	MsgType         int
	MsgDigest       [32]byte
	Signature       []byte
	PrepareSeqNum   int
	View            int
	ClientTimestamp int
	ServerId        int // XPaxos server that created the message
}

type PrepareLogEntry struct {
	Request ClientRequest
	Msg0    Message
}

type CommitLogEntry struct {
	Request ClientRequest
	Msg0    map[int]Message
}

//
// ------------------------------ HELPER FUNCTIONS -----------------------------
//
func checkError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func digest(msg interface{}) [32]byte { // Crypto message digest
	jsonBytes, _ := json.Marshal(msg)
	return sha256.Sum256(jsonBytes)
}

func generateKeys() (*rsa.PrivateKey, *rsa.PublicKey) { // Crypto RSA private/public key generation
	key, err := rsa.GenerateKey(rand.Reader, BITSIZE)
	checkError(err)
	return key, &key.PublicKey
}

func (xp *XPaxos) sign(msgDigest [32]byte) []byte { // Crypto message signature
	signature, err := rsa.SignPKCS1v15(rand.Reader, xp.privateKey, crypto.SHA256, msgDigest[:])
	checkError(err)
	return signature
}

func (xp *XPaxos) verify(server int, msgDigest [32]byte, signature []byte) bool { // Crypto signature verification
	err := rsa.VerifyPKCS1v15(xp.publicKeys[server], crypto.SHA256, msgDigest[:], signature)
	if err != nil {
		return false
	}
	return true
}

func (xp *XPaxos) getState() (int, bool) {
	xp.mu.Lock()
	defer xp.mu.Unlock()

	isLeader := false
	view := xp.view

	if xp.id == view {
		isLeader = true
	}

	return view, isLeader
}

func (xp *XPaxos) persist() {
	buf := new(bytes.Buffer)
	enc := gob.NewEncoder(buf)
	enc.Encode(0)
	data := buf.Bytes()
	xp.persister.SaveXPaxosState(data)
}

func (xp *XPaxos) readPersist(data []byte) {
	buf := bytes.NewBuffer(data)
	dec := gob.NewDecoder(buf)
	dec.Decode(0)
}

//
// ---------------------------- REPLICATE/REPLY RPC ---------------------------
//
func (xp *XPaxos) Replicate(request ClientRequest, reply *ReplicateReply) {
	if xp.id == xp.view { // If XPaxos server is the leader
		reply.IsLeader = true

		xp.mu.Lock()
		defer xp.mu.Unlock()

		xp.prepareSeqNum++
		msgDigest := digest(request)
		signature := xp.sign(msgDigest)

		msg := Message{
			MsgType:         PREPARE,
			MsgDigest:       msgDigest,
			Signature:       signature,
			PrepareSeqNum:   xp.prepareSeqNum,
			View:            xp.view,
			ClientTimestamp: request.Timestamp,
			ServerId:        xp.id}

		prepareEntry := PrepareLogEntry{
			Request: request,
			Msg0:    msg}

		xp.prepareLog = append(xp.prepareLog, prepareEntry)

		msgMap := make(map[int]Message, 0)
		msgMap[xp.id] = msg // Leader's prepare message

		commitEntry := CommitLogEntry{
			Request: request,
			Msg0:    msgMap}

		xp.commitLog = append(xp.commitLog, commitEntry)
		replyCh := make(chan bool, len(xp.synchronousGroup)-1)
		xp.mu.Unlock()

		for server, _ := range xp.synchronousGroup {
			if server != xp.id {
				go xp.issuePrepare(server, prepareEntry, replyCh)
			}
		}

		for i := 0; i < len(xp.synchronousGroup)-1; i++ {
			<-replyCh
		}

		xp.mu.Lock()
		xp.executeSeqNum++
		reply.Success = true
	} else {
		reply.IsLeader = false
		reply.Success = false
	}
}

//
// -------------------------------- PREPARE RPC -------------------------------
//
type PrepareReply struct {
	MsgDigest [32]byte
	Signature []byte
	Success   bool
}

func (xp *XPaxos) sendPrepare(server int, prepareEntry PrepareLogEntry, reply *PrepareReply) bool {
	DPrintf("Prepare: from XPaxos server (%d) to XPaxos server (%d)\n", xp.id, server)
	return xp.replicas[server].Call("XPaxos.Prepare", prepareEntry, reply)
}

func (xp *XPaxos) issuePrepare(server int, prepareEntry PrepareLogEntry, replyCh chan bool) {
	reply := &PrepareReply{}

	if ok := xp.sendPrepare(server, prepareEntry, reply); ok {
		if bytes.Compare(prepareEntry.Msg0.MsgDigest[:], reply.MsgDigest[:]) == 0 && xp.verify(server, reply.MsgDigest,
			reply.Signature) == true {
			if reply.Success == true {
				replyCh <- reply.Success
			} else {
				go xp.issuePrepare(server, prepareEntry, replyCh)
			}
		}
	}
}

func (xp *XPaxos) Prepare(prepareEntry PrepareLogEntry, reply *PrepareReply) {
	xp.mu.Lock()
	defer xp.mu.Unlock()

	msgDigest := digest(prepareEntry.Request)
	signature := xp.sign(msgDigest)
	reply.MsgDigest = msgDigest
	reply.Signature = signature

	if prepareEntry.Msg0.PrepareSeqNum == xp.prepareSeqNum+1 && bytes.Compare(prepareEntry.Msg0.MsgDigest[:],
		msgDigest[:]) == 0 && xp.verify(prepareEntry.Msg0.ServerId, msgDigest, prepareEntry.Msg0.Signature) == true {
		xp.prepareSeqNum++
		xp.prepareLog = append(xp.prepareLog, prepareEntry)

		msg := Message{
			MsgType:         COMMIT,
			MsgDigest:       msgDigest,
			Signature:       signature,
			PrepareSeqNum:   xp.prepareSeqNum,
			View:            xp.view,
			ClientTimestamp: prepareEntry.Request.Timestamp,
			ServerId:        xp.id}

		if xp.executeSeqNum >= len(xp.commitLog) {
			msgMap := make(map[int]Message, 0)
			msgMap[xp.view] = prepareEntry.Msg0 // Leader's prepare message
			msgMap[xp.id] = msg                 // Follower's commit message

			commitEntry := CommitLogEntry{
				Request: prepareEntry.Request,
				Msg0:    msgMap}

			xp.commitLog = append(xp.commitLog, commitEntry)
		}

		replyCh := make(chan bool, len(xp.synchronousGroup)-1)
		xp.mu.Unlock()

		for server, _ := range xp.synchronousGroup {
			if server != xp.id {
				go xp.issueCommit(server, msg, replyCh)
			}
		}

		for i := 0; i < len(xp.synchronousGroup)-1; i++ {
			<-replyCh
		}

		xp.mu.Lock()

		for len(xp.commitLog[xp.executeSeqNum].Msg0) != len(xp.synchronousGroup) {
			xp.mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			xp.mu.Lock()
		}

		xp.executeSeqNum++
		reply.Success = true
	} else {
		reply.Success = false
	}
}

//
// --------------------------------- COMMIT RPC --------------------------------
//
type CommitReply struct {
	MsgDigest [32]byte
	Signature []byte
	Success   bool
}

func (xp *XPaxos) sendCommit(server int, msg Message, reply *CommitReply) bool {
	DPrintf("Commit: from XPaxos server (%d) to XPaxos server (%d)\n", xp.id, server)
	return xp.replicas[server].Call("XPaxos.Commit", msg, reply)
}

func (xp *XPaxos) issueCommit(server int, msg Message, replyCh chan bool) {
	reply := &CommitReply{}

	if ok := xp.sendCommit(server, msg, reply); ok {
		if bytes.Compare(msg.MsgDigest[:], reply.MsgDigest[:]) == 0 && xp.verify(server, reply.MsgDigest,
			reply.Signature) == true {
			if reply.Success == true {
				replyCh <- true
			} else {
				go xp.issueCommit(server, msg, replyCh)
			}
		}
	}
}

func (xp *XPaxos) Commit(msg Message, reply *CommitReply) {
	xp.mu.Lock()
	defer xp.mu.Unlock()

	msgDigest := msg.MsgDigest
	signature := xp.sign(msgDigest)
	reply.MsgDigest = msgDigest
	reply.Signature = signature

	if xp.verify(msg.ServerId, msgDigest, msg.Signature) == true {
		if xp.executeSeqNum < len(xp.commitLog) {
			serverId := msg.ServerId
			xp.commitLog[xp.executeSeqNum].Msg0[serverId] = msg
			reply.Success = true
		} else {
			reply.Success = false
		}
	}
}

//
// ------------------------------- MAKE FUNCTION ------------------------------
//
func Make(replicas []*labrpc.ClientEnd, id int, persister *Persister, privateKey *rsa.PrivateKey,
	publicKeys map[int]*rsa.PublicKey) *XPaxos {
	xp := &XPaxos{}

	xp.mu.Lock()
	xp.persister = persister
	xp.replicas = replicas
	xp.synchronousGroup = make(map[int]bool, 0)
	xp.id = id
	xp.view = 1
	xp.prepareSeqNum = 0
	xp.executeSeqNum = 0
	xp.prepareLog = make([]PrepareLogEntry, 0)
	xp.commitLog = make([]CommitLogEntry, 0)
	xp.privateKey = privateKey
	xp.publicKeys = publicKeys

	for server, _ := range xp.replicas {
		if server != CLIENT {
			xp.synchronousGroup[server] = true
		}
	}

	xp.readPersist(persister.ReadXPaxosState())
	xp.mu.Unlock()

	return xp
}

func (xp *XPaxos) Kill() {}
