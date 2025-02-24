package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flashbots/go-boost-utils/types"
	"github.com/flashbots/go-utils/httplogger"
	"github.com/flashbots/mev-boost/config"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

var (
	errInvalidSlot               = errors.New("invalid slot")
	errInvalidHash               = errors.New("invalid hash")
	errInvalidPubkey             = errors.New("invalid pubkey")
	errNoSuccessfulRelayResponse = errors.New("no successful relay response")

	errServerAlreadyRunning = errors.New("server already running")
)

var nilHash = types.Hash{}
var nilResponse = struct{}{}

type httpErrorResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// BoostServiceOpts provides all available options for use with NewBoostService
type BoostServiceOpts struct {
	Log                   *logrus.Entry
	ListenAddr            string
	Relays                []RelayEntry
	GenesisForkVersionHex string
	RelayRequestTimeout   time.Duration
	RelayCheck            bool
}

// BoostService - the mev-boost service
type BoostService struct {
	listenAddr string
	relays     []RelayEntry
	log        *logrus.Entry
	srv        *http.Server
	relayCheck bool

	builderSigningDomain types.Domain
	httpClient           http.Client

	bidsLock sync.Mutex
	bids     map[bidRespKey]bidResp // keeping track of bids, to log the originating relay on withholding
}

// NewBoostService created a new BoostService
func NewBoostService(opts BoostServiceOpts) (*BoostService, error) {
	if len(opts.Relays) == 0 {
		return nil, errors.New("no relays")
	}

	builderSigningDomain, err := ComputeDomain(types.DomainTypeAppBuilder, opts.GenesisForkVersionHex, types.Root{}.String())
	if err != nil {
		return nil, err
	}

	return &BoostService{
		listenAddr: opts.ListenAddr,
		relays:     opts.Relays,
		log:        opts.Log.WithField("module", "service"),
		relayCheck: opts.RelayCheck,
		bids:       make(map[bidRespKey]bidResp),

		builderSigningDomain: builderSigningDomain,
		httpClient: http.Client{
			Timeout: opts.RelayRequestTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (m *BoostService) respondError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	resp := httpErrorResp{code, message}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		m.log.WithField("response", resp).WithError(err).Error("Couldn't write error response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func (m *BoostService) respondOK(w http.ResponseWriter, response any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		m.log.WithField("response", response).WithError(err).Error("Couldn't write OK response")
		http.Error(w, "", http.StatusInternalServerError)
	}
}

func (m *BoostService) getRouter() http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/", m.handleRoot)

	r.HandleFunc(pathStatus, m.handleStatus).Methods(http.MethodGet)
	r.HandleFunc(pathRegisterValidator, m.handleRegisterValidator).Methods(http.MethodPost)
	r.HandleFunc(pathGetHeader, m.handleGetHeader).Methods(http.MethodGet)
	r.HandleFunc(pathGetPayload, m.handleGetPayload).Methods(http.MethodPost)

	r.Use(mux.CORSMethodMiddleware(r))
	loggedRouter := httplogger.LoggingMiddlewareLogrus(m.log, r)
	return loggedRouter
}

// StartHTTPServer starts the HTTP server for this boost service instance
func (m *BoostService) StartHTTPServer() error {
	if m.srv != nil {
		return errServerAlreadyRunning
	}

	go m.startBidCacheCleanupTask()

	m.srv = &http.Server{
		Addr:    m.listenAddr,
		Handler: m.getRouter(),

		ReadTimeout:       time.Duration(config.ServerReadTimeoutMs) * time.Millisecond,
		ReadHeaderTimeout: time.Duration(config.ServerReadHeaderTimeoutMs) * time.Millisecond,
		WriteTimeout:      time.Duration(config.ServerWriteTimeoutMs) * time.Millisecond,
		IdleTimeout:       time.Duration(config.ServerIdleTimeoutMs) * time.Millisecond,

		MaxHeaderBytes: config.ServerMaxHeaderBytes,
	}

	err := m.srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (m *BoostService) startBidCacheCleanupTask() {
	for {
		time.Sleep(1 * time.Minute)
		m.bidsLock.Lock()
		for k, bidResp := range m.bids {
			if time.Since(bidResp.t) > 3*time.Minute {
				delete(m.bids, k)
			}
		}
		m.bidsLock.Unlock()
	}
}

func (m *BoostService) handleRoot(w http.ResponseWriter, req *http.Request) {
	m.respondOK(w, nilResponse)
}

// handleStatus sends calls to the status endpoint of every relay.
// It returns OK if at least one returned OK, and returns error otherwise.
func (m *BoostService) handleStatus(w http.ResponseWriter, req *http.Request) {
	if !m.relayCheck {
		m.respondOK(w, nilResponse)
		return
	}

	// If relayCheck is enabled, make sure at least 1 relay returns success
	var wg sync.WaitGroup
	var numSuccessRequestsToRelay uint32
	ua := UserAgent(req.Header.Get("User-Agent"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, r := range m.relays {
		wg.Add(1)

		go func(relay RelayEntry) {
			defer wg.Done()
			url := relay.GetURI(pathStatus)
			log := m.log.WithField("url", url)
			log.Debug("Checking relay status")

			_, err := SendHTTPRequest(ctx, m.httpClient, http.MethodGet, url, ua, nil, nil)
			if err != nil && ctx.Err() != context.Canceled {
				log.WithError(err).Error("failed to retrieve relay status")
				return
			}

			// Success: increase counter and cancel all pending requests to other relays
			atomic.AddUint32(&numSuccessRequestsToRelay, 1)
			cancel()
		}(r)
	}

	// At the end, wait for every routine and return status according to relay's ones.
	wg.Wait()

	if numSuccessRequestsToRelay > 0 {
		m.respondOK(w, nilResponse)
	} else {
		m.respondError(w, http.StatusServiceUnavailable, "all relays are unavailable")
	}
}

// handleRegisterValidator - returns 200 if at least one relay returns 200, else 502
func (m *BoostService) handleRegisterValidator(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "registerValidator")
	log.Debug("registerValidator")

	payload := []types.SignedValidatorRegistration{}
	if err := DecodeJSON(req.Body, &payload); err != nil {
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ua := UserAgent(req.Header.Get("User-Agent"))
	log = log.WithFields(logrus.Fields{
		"numRegistrations": len(payload),
		"ua":               ua,
	})

	relayRespCh := make(chan error, len(m.relays))

	for _, relay := range m.relays {
		go func(relay RelayEntry) {
			url := relay.GetURI(pathRegisterValidator)
			log := log.WithField("url", url)

			_, err := SendHTTPRequest(context.Background(), m.httpClient, http.MethodPost, url, ua, payload, nil)
			relayRespCh <- err
			if err != nil {
				log.WithError(err).Warn("error calling registerValidator on relay")
				return
			}
		}(relay)
	}

	for i := 0; i < len(m.relays); i++ {
		respErr := <-relayRespCh
		if respErr == nil {
			m.respondOK(w, nilResponse)
			return
		}
	}

	m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
}

// handleGetHeader requests bids from the relays
func (m *BoostService) handleGetHeader(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	slot := vars["slot"]
	parentHashHex := vars["parent_hash"]
	pubkey := vars["pubkey"]
	log := m.log.WithFields(logrus.Fields{
		"method":     "getHeader",
		"slot":       slot,
		"parentHash": parentHashHex,
		"pubkey":     pubkey,
	})
	log.Debug("getHeader")

	_slot, err := strconv.ParseUint(slot, 10, 64)
	if err != nil {
		m.respondError(w, http.StatusBadRequest, errInvalidSlot.Error())
		return
	}

	if len(pubkey) != 98 {
		m.respondError(w, http.StatusBadRequest, errInvalidPubkey.Error())
		return
	}

	if len(parentHashHex) != 66 {
		m.respondError(w, http.StatusBadRequest, errInvalidHash.Error())
		return
	}

	var mu sync.Mutex
	relays := make(map[string][]string) // relays per blockHash
	result := bidResp{}

	ua := UserAgent(req.Header.Get("User-Agent"))

	// Call the relays
	var wg sync.WaitGroup
	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			path := fmt.Sprintf("/eth/v1/builder/header/%s/%s/%s", slot, parentHashHex, pubkey)
			url := relay.GetURI(path)
			log := log.WithField("url", url)
			responsePayload := new(types.GetHeaderResponse)
			code, err := SendHTTPRequest(context.Background(), m.httpClient, http.MethodGet, url, ua, nil, responsePayload)
			if err != nil {
				log.WithError(err).Warn("error making request to relay")
				return
			}

			if code == http.StatusNoContent {
				log.Debug("no-content response")
				return
			}

			// Skip if invalid payload
			if responsePayload.Data == nil || responsePayload.Data.Message == nil || responsePayload.Data.Message.Header == nil || responsePayload.Data.Message.Header.BlockHash == nilHash {
				return
			}

			blockHash := responsePayload.Data.Message.Header.BlockHash.String()
			log = log.WithFields(logrus.Fields{
				"blockNumber": responsePayload.Data.Message.Header.BlockNumber,
				"blockHash":   blockHash,
				"txRoot":      responsePayload.Data.Message.Header.TransactionsRoot.String(),
				"value":       responsePayload.Data.Message.Value.String(),
			})

			if relay.PublicKey != responsePayload.Data.Message.Pubkey {
				log.Errorf("bid pubkey mismatch. expected: %s - got: %s", relay.PublicKey.String(), responsePayload.Data.Message.Pubkey.String())
				return
			}

			// Verify the relay signature in the relay response
			ok, err := types.VerifySignature(responsePayload.Data.Message, m.builderSigningDomain, relay.PublicKey[:], responsePayload.Data.Signature[:])
			if err != nil {
				log.WithError(err).Error("error verifying relay signature")
				return
			}
			if !ok {
				log.Error("failed to verify relay signature")
				return
			}

			// Verify response coherence with proposer's input data
			responseParentHash := responsePayload.Data.Message.Header.ParentHash.String()
			if responseParentHash != parentHashHex {
				log.WithFields(logrus.Fields{
					"originalParentHash": parentHashHex,
					"responseParentHash": responseParentHash,
				}).Error("proposer and relay parent hashes are not the same")
				return
			}

			isZeroValue := responsePayload.Data.Message.Value.String() == "0"
			isEmptyListTxRoot := responsePayload.Data.Message.Header.TransactionsRoot.String() == "0x7ffe241ea60187fdb0187bfa22de35d1f9bed7ab061d9401fd47e34a54fbede1"
			if isZeroValue || isEmptyListTxRoot {
				log.Warn("ignoring bid with 0 value")
				return
			}

			mu.Lock()
			defer mu.Unlock()

			// Remember which relays delivered which bids (multiple relays might deliver the top bid)
			if _, ok := relays[blockHash]; !ok {
				relays[blockHash] = []string{relay.String()}
			} else {
				relays[blockHash] = append(relays[blockHash], relay.String())
			}

			// Skip if value (fee) is not greater than the current highest value
			if result.response.Data != nil && responsePayload.Data.Message.Value.Cmp(&result.response.Data.Message.Value) < 1 {
				return
			}

			// Use this relay's response as mev-boost response because it's most profitable
			log.Debug("received a good bid")
			result.response = *responsePayload
			result.blockHash = blockHash
			result.t = time.Now()
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	if result.blockHash == "" {
		log.Info("no bid received")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Log result
	result.relays = relays[result.blockHash]
	log.WithFields(logrus.Fields{
		"blockHash":   result.blockHash,
		"blockNumber": result.response.Data.Message.Header.BlockNumber,
		"txRoot":      result.response.Data.Message.Header.TransactionsRoot.String(),
		"value":       result.response.Data.Message.Value.String(),
		"relays":      strings.Join(result.relays, ", "),
	}).Info("best bid")

	// Remember the bid, for future logging in case of withholding
	bidKey := bidRespKey{slot: _slot, blockHash: result.blockHash}
	m.bidsLock.Lock()
	m.bids[bidKey] = result
	m.bidsLock.Unlock()

	// Return the bid
	m.respondOK(w, result.response)
}

func (m *BoostService) handleGetPayload(w http.ResponseWriter, req *http.Request) {
	log := m.log.WithField("method", "getPayload")
	log.Debug("getPayload")

	payload := new(types.SignedBlindedBeaconBlock)
	if err := DecodeJSON(req.Body, &payload); err != nil {
		m.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	log = log.WithField("blockHash", payload.Message.Body.ExecutionPayloadHeader.BlockHash.String())
	var wg sync.WaitGroup
	var mu sync.Mutex
	result := new(types.GetPayloadResponse)
	ua := UserAgent(req.Header.Get("User-Agent"))

	// Prepare the request context, which will be cancelled after the first successful response from a relay
	requestCtx, requestCtxCancel := context.WithCancel(context.Background())
	defer requestCtxCancel()

	for _, relay := range m.relays {
		wg.Add(1)
		go func(relay RelayEntry) {
			defer wg.Done()
			url := relay.GetURI(pathGetPayload)
			log := log.WithField("url", url)
			log.Debug("calling getPayload")

			responsePayload := new(types.GetPayloadResponse)
			_, err := SendHTTPRequest(requestCtx, m.httpClient, http.MethodPost, url, ua, payload, responsePayload)

			if err != nil {
				log.WithError(err).Error("error making request to relay")
				return
			}

			if responsePayload.Data == nil || responsePayload.Data.BlockHash == nilHash {
				log.Error("response with empty data!")
				return
			}

			// Ensure the response blockhash matches the request
			if payload.Message.Body.ExecutionPayloadHeader.BlockHash != responsePayload.Data.BlockHash {
				log.WithFields(logrus.Fields{
					"responseBlockHash": responsePayload.Data.BlockHash.String(),
				}).Error("requestBlockHash does not equal responseBlockHash")
				return
			}

			// Lock before accessing the shared payload
			mu.Lock()
			defer mu.Unlock()

			if requestCtx.Err() != nil { // request has been cancelled (or deadline exceeded)
				return
			}

			// Received successful response. Now cancel other requests and return immediately
			requestCtxCancel()
			*result = *responsePayload
			log.Info("received payload from relay")
		}(relay)
	}

	// Wait for all requests to complete...
	wg.Wait()

	// If no payload has been received from relay, log loudly about withholding!
	if result.Data == nil || result.Data.BlockHash == nilHash {
		bidKey := bidRespKey{slot: payload.Message.Slot, blockHash: payload.Message.Body.ExecutionPayloadHeader.BlockHash.String()}
		m.bidsLock.Lock()
		originalResp := m.bids[bidKey]
		m.bidsLock.Unlock()
		log.WithField("relays", strings.Join(originalResp.relays, ", ")).Errorf("no payload received from relay -- withholding or network error --")
		m.respondError(w, http.StatusBadGateway, errNoSuccessfulRelayResponse.Error())
		return
	}

	m.respondOK(w, result)
}

// CheckRelays sends a request to each one of the relays previously registered to get their status
func (m *BoostService) CheckRelays() bool {
	for _, relay := range m.relays {
		m.log.WithField("relay", relay.String()).Info("Checking relay")

		url := relay.GetURI(pathStatus)
		_, err := SendHTTPRequest(context.Background(), m.httpClient, http.MethodGet, url, "", nil, nil)
		if err != nil {
			m.log.WithError(err).WithField("relay", relay.String()).Error("relay check failed")
			return false
		}
	}

	return true
}
