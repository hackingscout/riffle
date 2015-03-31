//package server
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/rpc"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"

	"time"

	. "afs/lib" //types and utils

	"github.com/dedis/crypto/abstract"
	"github.com/dedis/crypto/edwards"
	"github.com/dedis/crypto/proof"
	"github.com/dedis/crypto/shuffle"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/sha3"
)

var TotalClients = 0

//any variable/func with 2: similar object as s-c but only s-s
type Server struct {
	addr            string //this server
	port            int
	id              int
	servers         []string //other servers
	rpcServers      []*rpc.Client
	regLock         []*sync.Mutex //registration mutex
	regChan         chan bool
	regDone         chan bool
	connectDone     chan bool
	running         chan bool
	secretLock      *sync.Mutex

	//crypto
	suite           abstract.Suite
	g               abstract.Group
	sk              abstract.Secret //secret and public elgamal key
	pk              abstract.Point
	pkBin           []byte
	pks             []abstract.Point //all servers pks
	nextPks         []abstract.Point
	nextPksBin      [][]byte
	ephSecret       abstract.Secret

	//used during key shuffle
	pi              []int
	keys            [][]byte
	keysRdy         chan bool
	auxProofChan    []chan AuxKeyProof
	keyUploadChan   chan UpKey
	keyShuffleChan  chan InternalKey //collect all uploads together

	//clients
	clientMap       map[int]int //maps clients to dedicated server
	numClients      int //#clients connect here
	totalClients    int //total number of clients (sum of all servers)
	maskss          [][][]byte //clients' masks for PIR
	secretss        [][][]byte //shared secret used to xor

	//all rounds
	rounds          []*Round

	memProf         *os.File
}

//per round variables
type Round struct {
	allBlocks       []Block //all blocks store on this server

	//requesting
	reqChan2        []chan Request
	requestsChan    chan []Request
	reqHashes       [][]byte
	reqHashesRdy    []chan bool

	//uploading
	ublockChan2     []chan Block
	shuffleChan     chan []Block
	upHashesRdy     []chan bool

	//downloading
	upHashes        [][]byte
	dblocksChan     chan []Block
	blocksRdy       []chan bool
	xorsChan        []map[int](chan Block)
}

///////////////////////////////
//Initial Setup
//////////////////////////////

func NewServer(addr string, port int, id int, servers []string) *Server {
	suite := edwards.NewAES128SHA256Ed25519(false)
	rand := suite.Cipher(abstract.RandomKey)
	sk := suite.Secret().Pick(rand)
	pk := suite.Point().Mul(nil, sk)
	pkBin := MarshalPoint(pk)
	ephSecret := suite.Secret().Pick(rand)

	rounds := make([]*Round, MaxRounds)

	for i := range rounds {
		r := Round{
			allBlocks:      nil,

			reqChan2:       nil,
			requestsChan:   nil,
			reqHashes:      nil,
			reqHashesRdy:   nil,

			ublockChan2:    nil,
			shuffleChan:    make(chan []Block), //collect all uploads together
			upHashesRdy:    nil,

			upHashes:       nil,
			dblocksChan:    make(chan []Block),
			blocksRdy:      nil,
			xorsChan:       make([]map[int](chan Block), len(servers)),
		}
		rounds[i] = &r
	}

	s := Server{
		addr:           addr,
		port:           port,
		id:             id,
		servers:        servers,
		regLock:        []*sync.Mutex{new(sync.Mutex), new(sync.Mutex)},
		regChan:        make(chan bool, TotalClients),
		regDone:        make(chan bool),
		connectDone:    make(chan bool),
		running:        make(chan bool),
		secretLock:     new(sync.Mutex),

		suite:          suite,
		g:              suite,
		sk:             sk,
		pk:             pk,
		pkBin:          pkBin,
		pks:            make([]abstract.Point, len(servers)),
		nextPks:        make([]abstract.Point, len(servers)),
		nextPksBin:     make([][]byte, len(servers)),
		ephSecret:      ephSecret,

		pi:             nil,
		keys:           nil,
		keysRdy:        nil,
		auxProofChan:   make([]chan AuxKeyProof, len(servers)),
		keyUploadChan:  nil,
		keyShuffleChan: make(chan InternalKey),

		clientMap:      make(map[int]int),
		numClients:     0,
		totalClients:   0,
		maskss:         nil,
		secretss:       nil,

		rounds:         rounds,

		memProf:        nil,
	}

	for i := range s.auxProofChan {
		s.auxProofChan[i] = make(chan AuxKeyProof, len(servers))
	}

	return &s
}


/////////////////////////////////
//Helpers
////////////////////////////////

func (s *Server) runHandlers() {
	//<-s.connectDone
	<-s.regDone

	runHandler(s.gatherKeys, 1)
	runHandler(s.shuffleKeys, 1)

	runHandler(s.gatherRequests, MaxRounds)
	runHandler(s.shuffleRequests, MaxRounds)
	runHandler(s.gatherUploads, MaxRounds)
	runHandler(s.shuffleUploads, MaxRounds)
	runHandler(s.handleResponses, MaxRounds)

	s.running <- true
}

func (s *Server) gatherRequests(round uint64) {
	rnd := round % MaxRounds
	allReqs := make([]Request, s.totalClients)
	var wg sync.WaitGroup
	for i := 0; i < s.totalClients; i++ {
		wg.Add(1)
		go func (i int) {
			defer wg.Done()
			req := <-s.rounds[rnd].reqChan2[i]
			req.Id = 0
			allReqs[i] = req
		} (i)
	}
	wg.Wait()

	s.rounds[rnd].requestsChan <- allReqs
}

func (s *Server) shuffleRequests(round uint64) {
	rnd := round % MaxRounds
	allReqs := <-s.rounds[rnd].requestsChan

	//construct permuted blocks
	input := make([][]byte, s.totalClients)
	for i := range input {
		input[i] = allReqs[s.pi[i]].Hash
	}

	s.shuffle(input, round)

	reqs := make([]Request, s.totalClients)
	for i := range reqs {
		reqs[i] = Request{Hash: input[i], Round: round, Id: 0}
	}

	t := time.Now()
	if s.id == len(s.servers) - 1 {
		var wg sync.WaitGroup
		for _, rpcServer := range s.rpcServers {
			wg.Add(1)
			go func(rpcServer *rpc.Client) {
				defer wg.Done()
				err := rpcServer.Call("Server.PutPlainRequests", &reqs, nil)
				if err != nil {
					log.Fatal("Failed uploading shuffled and decoded reqs: ", err)
				}
			} (rpcServer)
		}
		wg.Wait()
	} else {
		err := s.rpcServers[s.id+1].Call("Server.ShareServerRequests", &reqs, nil)
		if err != nil {
			log.Fatal("Couldn't hand off the requests to next server", s.id+1, err)
		}
	}

	fmt.Println("round", round, ". ", s.id, "server shuffle req: ", time.Since(t))
}

func (s *Server) handleResponses(round uint64) {
	rnd := round % MaxRounds
	allBlocks := <-s.rounds[rnd].dblocksChan
	//store it on this server as well
	s.rounds[rnd].allBlocks = allBlocks

	t := time.Now()

	var wg sync.WaitGroup
	for i := 0; i < s.totalClients; i++ {
		if s.clientMap[i] == s.id {
			continue
		}
		//if it doesnt belong to me, xor things and send it over
		wg.Add(1)
		go func(i int, rpcServer *rpc.Client, r uint64) {
			defer wg.Done()
			res := ComputeResponse(allBlocks, s.maskss[r][i], s.secretss[r][i])
			sha3.ShakeSum256(s.secretss[r][i], s.secretss[r][i])
			sha3.ShakeSum256(s.maskss[r][i], s.maskss[r][i])
			//fmt.Println(s.id, round, "mask", i, s.maskss[i])
			cb := ClientBlock {
				CId: i,
				SId: s.id,
				Block: Block {
					Block: res,
					Round: round,
				},
			}
			err := rpcServer.Call("Server.PutClientBlock", cb, nil)
			if err != nil {
				log.Fatal("Couldn't put block: ", err)
			}
		} (i, s.rpcServers[s.clientMap[i]], rnd)
	}
	wg.Wait()

	fmt.Println(s.id, "handling_resp:", time.Since(t))

	for i := range s.rounds[rnd].blocksRdy {
		if s.clientMap[i] != s.id {
			continue
		}
		go func(i int, round uint64) {
			s.rounds[rnd].blocksRdy[i] <- true
		} (i, round)
	}
}

func (s *Server) gatherUploads(round uint64) {
	rnd := round % MaxRounds
	allBlocks := make([]Block, s.totalClients)
	var wg sync.WaitGroup
	for i := 0; i < s.totalClients; i++ {
		wg.Add(1)
		go func (i int) {
			defer wg.Done()
			block := <-s.rounds[rnd].ublockChan2[i]
			block.Id = 0
			allBlocks[i] = block
		} (i)
	}
	wg.Wait()

	s.rounds[rnd].shuffleChan <- allBlocks
}

func (s *Server) shuffleUploads(round uint64) {
	rnd := round % MaxRounds
	allBlocks := <-s.rounds[rnd].shuffleChan

	//construct permuted blocks
	input := make([][]byte, s.totalClients)
	for i := range input {
		input[i] = allBlocks[s.pi[i]].Block
	}

	s.shuffle(input, round)

	uploads := make([]Block, s.totalClients)
	for i := range uploads {
		uploads[i] = Block{Block: input[i], Round: round, Id: 0}
	}

	t := time.Now()

	if s.id == len(s.servers) - 1 {
		var wg sync.WaitGroup
		for _, rpcServer := range s.rpcServers {
			wg.Add(1)
			go func(rpcServer *rpc.Client) {
				defer wg.Done()
				err := rpcServer.Call("Server.PutPlainBlocks", &uploads, nil)
				if err != nil {
					log.Fatal("Failed uploading shuffled and decoded blocks: ", err)
				}
			} (rpcServer)
		}
		wg.Wait()
	} else {
		err := s.rpcServers[s.id+1].Call("Server.ShareServerBlocks", &uploads, nil)
		if err != nil {
			log.Fatal("Couldn't hand off the blocks to next server", s.id+1, err)
		}
	}
	fmt.Println("round", round, ". ", s.id, "server shuffle: ", time.Since(t))
}

func (s *Server) gatherKeys(_ uint64) {
	allKeys := make([]UpKey, s.totalClients)
	for i := 0; i < s.totalClients; i++ {
		key := <-s.keyUploadChan
		allKeys[key.Id] = key
	}

	serversLeft := len(s.servers)-s.id

	Xss := make([][][]byte, serversLeft)
	Yss := make([][][]byte, serversLeft)

	for i := range Xss {
		Xss[i] = make([][]byte, s.totalClients)
		Yss[i] = make([][]byte, s.totalClients)
		for j := range Xss[i] {
			Xss[i][j] = allKeys[j].C1s[i]
			Yss[i][j] = allKeys[j].C2s[i]
		}
	}

	ik := InternalKey {
		Xss: append([][][]byte{nil}, Xss ...),
		Yss: append([][][]byte{nil}, Yss ...),
		SId: s.id,
	}

	aux := AuxKeyProof {
		OrigXss: Xss,
		OrigYss: Yss,
		SId:     s.id,
	}

	var wg sync.WaitGroup
	for _, rpcServer := range s.rpcServers {
		wg.Add(1)
		go func(rpcServer *rpc.Client) {
			defer wg.Done()
			err := rpcServer.Call("Server.PutAuxProof", &aux, nil)
			if err != nil {
				log.Fatal("Failed uploading shuffled and decoded blocks: ", err)
			}
		} (rpcServer)
	}
	wg.Wait()

	//fmt.Println(s.id, "done gathering")
	s.keyShuffleChan <- ik
}

func (s *Server) shuffleKeys(_ uint64) {
	keys := <-s.keyShuffleChan

	//fmt.Println(s.id, "shuffle start: ", round)

	//shuffle and reblind

	serversLeft := len(s.servers)-s.id
	// for _, upload := range allKeys {
	// 	if hashChunks != len(upload.HC1[0])  {
	// 		panic("Different chunk lengths")
	// 	}
	// }

	Xss := make([][]abstract.Point, serversLeft)
	Yss := make([][]abstract.Point, serversLeft)
	for i := range Xss {
		Xss[i] = make([]abstract.Point, s.totalClients)
		Yss[i] = make([]abstract.Point, s.totalClients)
		for j := range Xss[i] {
			Xss[i][j] = UnmarshalPoint(s.suite, keys.Xss[i+1][j])
			Yss[i][j] = UnmarshalPoint(s.suite, keys.Yss[i+1][j])
		}
	}

	Xbarss := make([][]abstract.Point, serversLeft)
	Ybarss := make([][]abstract.Point, serversLeft)
	decss := make([][]abstract.Point, serversLeft)
	prfs := make([][]byte, serversLeft)

	var shuffleWG sync.WaitGroup
	for i := 0; i < serversLeft; i++ {
		shuffleWG.Add(1)
		go func(i int, pk abstract.Point) {
			defer shuffleWG.Done()
			//only one chunk
			rand := s.suite.Cipher(abstract.RandomKey)
			var prover proof.Prover
			var err error
			Xbarss[i], Ybarss[i], prover = shuffle.Shuffle2(s.pi, s.g, nil, pk, Xss[i], Yss[i], rand)
			prfs[i], err = proof.HashProve(s.suite, "PairShuffle", rand, prover)
			if err != nil {
				log.Fatal("Shuffle proof failed: " + err.Error())
			}
			var decWG sync.WaitGroup
			decss[i] = make([]abstract.Point, s.totalClients)
			for j := range decss[i] {
				decWG.Add(1)
				go func (i int, j int) {
					defer decWG.Done()
					decss[i][j] = Decrypt(s.g, Xbarss[i][j], Ybarss[i][j], s.sk)
				} (i, j)
			}
			decWG.Wait()

		} (i, s.nextPks[i])
	}
	shuffleWG.Wait()

	//whatever is at index 0 belongs to me
	for i := range decss[0] {
		s.keys[i] = MarshalPoint(decss[0][i])
	}

	ik := InternalKey {
		Xss: make([][][]byte, serversLeft),
		Yss: make([][][]byte, serversLeft),
		SId: s.id,

		Ybarss:  make([][][]byte, serversLeft),
		Proofs:  prfs,
		Keys:    make([][]byte, serversLeft),
	}

	for i := range ik.Xss {
		ik.Xss[i] = make([][]byte, s.totalClients)
		ik.Yss[i] = make([][]byte, s.totalClients)
		ik.Ybarss[i] = make([][]byte, s.totalClients)
		for j := range ik.Xss[i] {
			ik.Xss[i][j] = MarshalPoint(Xbarss[i][j])
			if i == 0 {
				//i == 0 is my point, so don't pass it to next person
				ik.Yss[i][j] = MarshalPoint(s.g.Point().Base())
			} else {
				ik.Yss[i][j] = MarshalPoint(decss[i][j])
			}
			ik.Ybarss[i][j] = MarshalPoint(Ybarss[i][j])
		}
		ik.Keys[i] = s.nextPksBin[i]
	}

	var wg sync.WaitGroup
	for _, rpcServer := range s.rpcServers {
		wg.Add(1)
		go func(rpcServer *rpc.Client) {
			defer wg.Done()
			err := rpcServer.Call("Server.ShareServerKeys", &ik, nil)
			if err != nil {
				log.Fatal("Failed uploading shuffled and decoded blocks: ", err)
			}
		} (rpcServer)
	}
	wg.Wait()

	fmt.Println(s.id, "shuffle done")
}

/////////////////////////////////
//Registration and Setup
////////////////////////////////
//register the client here, and notify the server it will be talking to
//TODO: should check for duplicate clients, just in case..
func (s *Server) Register(serverId int, clientId *int) error {
	s.regLock[0].Lock()
	*clientId = s.totalClients
	client := &ClientRegistration{
		ServerId: serverId,
		Id: *clientId,
	}
	s.totalClients++
	for _, rpcServer := range s.rpcServers {
		err := rpcServer.Call("Server.Register2", client, nil)
		if err != nil {
			log.Fatal(fmt.Sprintf("Cannot connect to %d: ", serverId), err)
		}
	}
	if s.totalClients == TotalClients {
		s.registerDone()
	}
	fmt.Println("Registered", *clientId)
	s.regLock[0].Unlock()
	return nil
}

//called to increment total number of clients
func (s *Server) Register2(client *ClientRegistration, _ *int) error {
	s.regLock[1].Lock()
	s.clientMap[client.Id] = client.ServerId
	s.regLock[1].Unlock()
	return nil
}

func (s *Server) registerDone() {
	for _, rpcServer := range s.rpcServers {
		err := rpcServer.Call("Server.RegisterDone2", s.totalClients, nil)
		if err != nil {
			log.Fatal("Cannot update num clients")
		}
	}

	for i := 0; i < s.totalClients; i++ {
		s.regChan <- true
	}
}

func (s *Server) RegisterDone2(numClients int, _ *int) error {
	s.totalClients = numClients

	size := (numClients/SecretSize)*SecretSize + SecretSize
	s.maskss = make([][][]byte, MaxRounds)
	s.secretss = make([][][]byte, MaxRounds)
	for r := range s.maskss {
		s.maskss[r] = make([][]byte, numClients)
		s.secretss[r] = make([][]byte, numClients)
		for i := range s.maskss[r] {
			s.maskss[r][i] = make([]byte, size)
			s.secretss[r][i] = make([]byte, BlockSize)
		}
	}

	rand := s.suite.Cipher(abstract.RandomKey)
	s.pi = GeneratePI(numClients, rand)

	s.keys = make([][]byte, numClients)
	s.keysRdy = make(chan bool, numClients)

	s.keyUploadChan = make(chan UpKey, numClients)

	for r := range s.rounds {
		for i := 0; i < len(s.servers); i++ {
			s.rounds[r].xorsChan[i] = make(map[int](chan Block))
			for j := 0; j < numClients; j++ {
				s.rounds[r].xorsChan[i][j] = make(chan Block)
			}
		}

		s.rounds[r].requestsChan = make(chan []Request)
		s.rounds[r].reqHashes = make([][]byte, numClients)

		s.rounds[r].reqChan2 = make([]chan Request, numClients)
		s.rounds[r].upHashes = make([][]byte, numClients)
		s.rounds[r].blocksRdy = make([]chan bool, numClients)
		s.rounds[r].upHashesRdy = make([]chan bool, numClients)
		s.rounds[r].reqHashesRdy = make([]chan bool, numClients)
		s.rounds[r].ublockChan2 = make([]chan Block, numClients)
		for i := range s.rounds[r].blocksRdy {
			s.rounds[r].reqChan2[i] = make(chan Request)
			s.rounds[r].blocksRdy[i] = make(chan bool)
			s.rounds[r].upHashesRdy[i] = make(chan bool)
			s.rounds[r].reqHashesRdy[i] = make(chan bool)
			s.rounds[r].ublockChan2[i] = make(chan Block)
		}
	}
	s.regDone <- true
	fmt.Println(s.id, "Register done")
	<-s.running
	return nil
}

func (s *Server) connectServers() {
	rpcServers := make([]*rpc.Client, len(s.servers))
	for i := range rpcServers {
		var rpcServer *rpc.Client
		var err error = errors.New("")
		for ; err != nil ; {
			if i == s.id { //make a local rpc
				addr := fmt.Sprintf("127.0.0.1:%d", s.port)
				rpcServer, err = rpc.Dial("tcp", addr)
			} else {
				rpcServer, err = rpc.Dial("tcp", s.servers[i])
			}
		}
		rpcServers[i] = rpcServer
	}

	var wg sync.WaitGroup
	for i, rpcServer := range rpcServers {
		wg.Add(1)
		go func (i int, rpcServer *rpc.Client) {
			defer wg.Done()
			pk := make([]byte, SecretSize)
			err := rpcServer.Call("Server.GetPK", 0, &pk)
			if err != nil {
				log.Fatal("Couldn't get server's pk: ", err)
			}
			s.pks[i] = UnmarshalPoint(s.suite, pk)
		} (i, rpcServer)
	}
	wg.Wait()
	for i := 0; i < len(s.servers)-s.id; i++ {
		pk := s.pk
		for j := 1; j <= i; j++ {
			pk = s.g.Point().Add(pk, s.pks[s.id + j])
		}
		s.nextPks[i] = pk
		s.nextPksBin[i] = MarshalPoint(pk)
	}
	s.rpcServers = rpcServers
	//s.connectDone <- true
}

func (s *Server) GetNumClients(_ int, num *int) error {
	<-s.regChan
	*num = s.totalClients
	return nil
}

func (s *Server) GetPK(_ int, pk *[]byte) error {
	*pk = s.pkBin
	return nil
}

func (s *Server) UploadKeys(key *UpKey, _*int) error {
	s.keyUploadChan <- *key
	return nil
}

func (s *Server) shareSecret(clientPublic abstract.Point) (abstract.Point, abstract.Point) {
	s.secretLock.Lock()
	rand := s.suite.Cipher(abstract.RandomKey)
	gen := s.g.Point().Base()
	secret := s.g.Secret().Pick(rand)
	public := s.g.Point().Mul(gen, secret)
	sharedSecret := s.g.Point().Mul(clientPublic, secret)
	s.secretLock.Unlock()
	return public, sharedSecret
}

func (s *Server) ShareMask(clientDH *ClientDH, serverPub *[]byte) error {
	pub, shared := s.shareSecret(UnmarshalPoint(s.suite, clientDH.Public))
	mask := MarshalPoint(shared)
	var rand abstract.Cipher
	for r := 0; r < MaxRounds; r++ {
		if r == 0 {
			rand = s.suite.Cipher(mask)
		} else {
			rand = s.suite.Cipher(s.maskss[r-1][clientDH.Id])
		}
		rand.Read(s.maskss[r][clientDH.Id])
	}
	*serverPub = MarshalPoint(pub)
	return nil
}

func (s *Server) ShareSecret(clientDH *ClientDH, serverPub *[]byte) error {
	pub, shared := s.shareSecret(UnmarshalPoint(s.suite, clientDH.Public))
	secret := MarshalPoint(shared)
	var rand abstract.Cipher
	for r := 0; r < MaxRounds; r++ {
		if r == 0 {
			rand = s.suite.Cipher(secret)
		} else {
			rand = s.suite.Cipher(s.secretss[r-1][clientDH.Id])
		}
		rand.Read(s.secretss[r][clientDH.Id])
	}
	//s.secretss[clientDH.Id] = make([]byte, len(MarshalPoint(shared)))
	*serverPub = MarshalPoint(pub)
	return nil
}

func (s *Server) GetEphKey(_ int, serverPub *[]byte) error {
	pub := s.g.Point().Mul(s.g.Point().Base(), s.ephSecret)
	*serverPub = MarshalPoint(pub)
	return nil
}

func (s *Server) PutAuxProof(aux *AuxKeyProof, _ *int) error {
	s.auxProofChan[aux.SId] <- *aux
	return nil
}

func (s *Server) ShareServerKeys(ik *InternalKey, correct *bool) error {
	aux := <-s.auxProofChan[ik.SId]
	//fmt.Println(s.id, "aux")
	good := s.verifyShuffle(*ik, aux)

	if ik.SId != len(s.servers) - 1 {
		aux = AuxKeyProof {
			OrigXss: ik.Xss[1:],
			OrigYss: ik.Yss[1:],
			SId:     ik.SId + 1,
		}
		s.auxProofChan[aux.SId] <- aux
	}

	if ik.SId == len(s.servers) - 1 && s.id == 0 {
		for i := 0; i < s.totalClients; i++ {
			go func () {
				s.keysRdy <- true
			} ()
		}
	} else if ik.SId == s.id - 1 {
		ik.Ybarss = nil
		ik.Proofs = nil
		ik.Keys = nil
		s.keyShuffleChan <- *ik
	}
	*correct = good
	return nil
}

func (s *Server) KeyReady(id int, _ *int) error {
	<-s.keysRdy
	return nil
}

/////////////////////////////////
//Request
////////////////////////////////
func (s *Server) RequestBlock(r *Request, _ *int) error {
	err := s.rpcServers[0].Call("Server.RequestBlock2", r, nil)
	return err
}

func (s *Server) RequestBlock2(r *Request, _ *int) error {
	round := r.Round % MaxRounds
	s.rounds[round].reqChan2[r.Id] <- *r
	return nil
}

func (s *Server) PutPlainRequests(rs *[]Request, _ *int) error {
	reqs := *rs
	round := reqs[0].Round % MaxRounds
	for i := range reqs {
		s.rounds[round].reqHashes[i] = reqs[i].Hash
	}

	for i := range s.rounds[round].reqHashesRdy {
		if s.clientMap[i] != s.id {
			continue
		}
		go func(i int, round uint64) {
			s.rounds[round].reqHashesRdy[i] <- true
		} (i, round)
	}

	return nil
}

func (s *Server) GetReqHashes(args *RequestArg, hashes *[][]byte) error {
	round := args.Round % MaxRounds
	<-s.rounds[round].reqHashesRdy[args.Id]
	*hashes = s.rounds[round].reqHashes
	return nil
}

func (s *Server) ShareServerRequests(reqs *[]Request, _ *int) error {
	round := (*reqs)[0].Round % MaxRounds
	s.rounds[round].requestsChan <- *reqs
	return nil
}

/////////////////////////////////
//Upload
////////////////////////////////
func (s *Server) UploadBlock(block *Block, _ *int) error {
	err := s.rpcServers[0].Call("Server.UploadBlock2", block, nil)
	if err != nil {
		log.Fatal("Couldn't send block to first server: ", err)
	}
	return nil
}

func (s *Server) UploadBlock2(block *Block, _*int) error {
	round := block.Round % MaxRounds
	s.rounds[round].ublockChan2[block.Id] <- *block
	//fmt.Println("put ublockchan2", round)
	return nil
}

func (s *Server) PutPlainBlocks(bs *[]Block, _ *int) error {
	blocks := *bs
	round := blocks[0].Round % MaxRounds

	for i := range blocks {
		h := s.suite.Hash()
		h.Write(blocks[i].Block)
		s.rounds[round].upHashes[i] = h.Sum(nil)
	}

	for i := range s.rounds[round].upHashesRdy {
		if s.clientMap[i] != s.id {
			continue
		}
		go func(i int, round uint64) {
			s.rounds[round].upHashesRdy[i] <- true
		} (i, round)
	}

	s.rounds[round].dblocksChan <- blocks

	return nil
}

func (s *Server) ShareServerBlocks(blocks *[]Block, _ *int) error {
	round := (*blocks)[0].Round % MaxRounds
	s.rounds[round].shuffleChan <- *blocks
	return nil
}


/////////////////////////////////
//Download
////////////////////////////////
func (s *Server) GetUpHashes(args *RequestArg, hashes *[][]byte) error {
	round := args.Round % MaxRounds
	<-s.rounds[round].upHashesRdy[args.Id]
	*hashes = s.rounds[round].upHashes
	return nil
}

func (s *Server) GetResponse(cmask ClientMask, response *[]byte) error {
	t := time.Now()
	round := cmask.Round % MaxRounds
	otherBlocks := make([][]byte, len(s.servers))
	var wg sync.WaitGroup
	for i := range otherBlocks {
		if i == s.id {
			otherBlocks[i] = make([]byte, BlockSize)
		} else {
			wg.Add(1)
			go func(i int, cmask ClientMask) {
				defer wg.Done()
				curBlock := <-s.rounds[round].xorsChan[i][cmask.Id]
				//fmt.Println(s.id, "mask for", cmask.Id, cmask.Mask)
				otherBlocks[i] = curBlock.Block
			} (i, cmask)
		}
	}
	wg.Wait()
	<-s.rounds[round].blocksRdy[cmask.Id]
	if cmask.Id == 0 {
		fmt.Println(cmask.Id, "down_network:", time.Since(t))
	}

	r := ComputeResponse(s.rounds[round].allBlocks, cmask.Mask, s.secretss[round][cmask.Id])
	sha3.ShakeSum256(s.secretss[round][cmask.Id], s.secretss[round][cmask.Id])
	Xor(Xors(otherBlocks), r)
	*response = r
	return nil
}

//used to push response for particular client
func (s *Server) PutClientBlock(cblock ClientBlock, _ *int) error {
	block := cblock.Block
	round := block.Round % MaxRounds
	s.rounds[round].xorsChan[cblock.SId][cblock.CId] <- block
	return nil
}

/////////////////////////////////
//Misc
////////////////////////////////
//used for the local test function to start the server
func (s *Server) MainLoop(_ int, _ *int) error {
	rpcServer := rpc.NewServer()
	rpcServer.Register(s)
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		panic("Cannot starting listening to the port")
	}
	go rpcServer.Accept(l)
	s.connectServers()
	go s.runHandlers()

	return nil
}


func (s *Server) verifyShuffle(ik InternalKey, aux AuxKeyProof) bool {
	Xss := aux.OrigXss
	Yss := aux.OrigYss
	Xbarss := ik.Xss
	Ybarss := ik.Ybarss
	prfss := ik.Proofs

	for i := range Xss {
		pk := UnmarshalPoint(s.suite, ik.Keys[i])
		Xs := make([]abstract.Point, len(Xss[i]))
		Ys := make([]abstract.Point, len(Yss[i]))
		Xbars := make([]abstract.Point, len(Xbarss[i]))
		Ybars := make([]abstract.Point, len(Ybarss[i]))
		for j := range Xss[i] {
			Xs[j] = UnmarshalPoint(s.suite, Xss[i][j])
			Ys[j] = UnmarshalPoint(s.suite, Yss[i][j])
			Xbars[j] = UnmarshalPoint(s.suite, Xbarss[i][j])
			Ybars[j] = UnmarshalPoint(s.suite, Ybarss[i][j])
		}
		v := shuffle.Verifier(s.suite, nil, pk, Xs, Ys, Xbars, Ybars)
		err := proof.HashVerify(s.suite, "PairShuffle", v, prfss[i])
		if err != nil {
			log.Println("Shuffle verify failed: ", err)
			return false
		}
	}
	return true
}

func (s *Server) shuffle(input [][]byte, round uint64) {
	tmp := make([]byte, 24)
	nonce := [24]byte{}
	binary.PutUvarint(tmp, round)
	copy(nonce[:], tmp[:])

	var aesWG sync.WaitGroup
	for i := 0; i < s.totalClients; i++ {
		aesWG.Add(1)
		go func(i int) {
			defer aesWG.Done()
			key := [32]byte{}
			copy(key[:], s.keys[i][:])
			var good bool
			input[i], good = secretbox.Open(nil, input[i], &nonce, &key)
			if !good {
				log.Fatal(round, "Check failed:", s.id, i)
			}
		} (i)
	}
	aesWG.Wait()
}

func (s *Server) Masks() [][][]byte {
	return s.maskss
}

func (s *Server) Secrets() [][][]byte {
	return s.secretss
}

func (s *Server) Keys() [][]byte {
	return s.keys
}

func runHandler(f func(uint64), rounds uint64) {
	var r uint64 = 0
	for ; r < rounds; r++ {
		go func (r uint64) {
			for {
				f(r)
				r += rounds
			}
		} (r)
	}
}

func SetTotalClients(n int) {
	TotalClients = n
}

/////////////////////////////////
//MAIN
/////////////////////////////////
func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")
	var memprofile = flag.String("memprofile", "", "write memory profile to this file")
	var id *int = flag.Int("i", 0, "id [num]")
	var servers *string = flag.String("s", "", "servers [file]")
	var numClients *int = flag.Int("n", 0, "num clients [num]")
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	ss := ParseServerList(*servers)

	SetTotalClients(*numClients)

	s := NewServer(ss[*id], ServerPort + *id, *id, ss)

	if *memprofile != "" {
                f, err := os.Create(*memprofile)
                if err != nil {
                        log.Fatal(err)
                }
                s.memProf = f
        }

	rpcServer := rpc.NewServer()
	rpcServer.Register(s)
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		panic("Cannot starting listening to the port")
	}

	go rpcServer.Accept(l)
	s.connectServers()
	fmt.Println("Starting server", *id)
	s.runHandlers()
	fmt.Println("Handler running", *id)

	Wait()
}

