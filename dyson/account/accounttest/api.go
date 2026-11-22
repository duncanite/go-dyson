// Package accounttest is Dyson's account API for tests: the login by a
// one-time code and the device list, answering as the open
// implementations read them, with an account and devices of its own.
package accounttest

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// The account the API holds, and the code it sends.
const (
	Email    = "owner@example.test"
	Password = "account-pass" // #nosec G101 -- the fake's own password, of no account
	Code     = "123456"
	Country  = "GB"
)

// Device is one device of the account, as the test says it.
type Device struct {
	Serial  string
	Name    string
	Product string
	Variant string
	// Credential is the broker's password; empty is a device the list
	// gives no local credential for.
	Credential string
	// Sealed, when not empty, is what the list gives as the local
	// credential in place of Credential encrypted.
	Sealed string
}

// Request is one request the API received.
type Request struct {
	Method        string
	Path          string
	Query         string
	Agent         string
	Authorization string
	Body          string
}

// API is the fake, serving until Close.
type API struct {
	*httptest.Server

	mu sync.Mutex
	// Status is what the API calls the account; empty is ACTIVE.
	Status string
	// TooOften has the API refuse to send a code, with a 429.
	TooOften bool
	// Fail, when not zero, is the status every route answers with, and
	// Location where it says to go when that status is a redirect.
	Fail     int
	Location string
	// Stall has every route take the request and never answer it.
	Stall bool
	// Raw, when not nil, is what the device list answers, under a 200.
	Raw []byte
	// Devices is the account's devices.
	Devices []Device

	codes       int
	requests    []Request
	provisioned bool
	challenge   string
	token       string
}

// New starts an API holding one account and no device. The caller closes
// it.
func New() *API {
	api := &API{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/provisioningservice/application/Android/version", api.provision)
	mux.HandleFunc("POST /v3/userregistration/email/userstatus", api.guarded(api.status))
	mux.HandleFunc("POST /v3/userregistration/email/auth", api.guarded(api.code))
	mux.HandleFunc("POST /v3/userregistration/email/verify", api.guarded(api.verify))
	mux.HandleFunc("GET /v2/provisioningservice/manifest", api.guarded(api.devices))

	api.Server = httptest.NewServer(mux)

	return api
}

// Received is the requests so far, and how many codes were sent.
func (a *API) Received() ([]Request, int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]Request(nil), a.requests...), a.codes
}

// Seal encrypts a broker's password as the list gives a local credential.
func Seal(credential string) string {
	plain, err := json.Marshal(map[string]any{"serial": "the device's own", "apPasswordHash": credential})
	if err != nil {
		panic(err)
	}

	padding := aes.BlockSize - len(plain)%aes.BlockSize

	return SealBlocks(append(plain, bytes.Repeat([]byte{byte(padding)}, padding)...))
}

// SealBlocks encrypts whole blocks as they are, with no padding added:
// for a local credential that is not what Seal makes.
func SealBlocks(plain []byte) string {
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}

	sealed := make([]byte, len(plain))

	// #nosec G407 -- Dyson's own scheme, reproduced: the vector is zeros for every device
	cipher.NewCBCEncrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(sealed, plain)

	return base64.StdEncoding.EncodeToString(sealed)
}

// record takes note of a request and returns its body. Called with the
// lock held.
func (a *API) record(req *http.Request) []byte {
	body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))

	a.requests = append(a.requests, Request{
		Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery, Agent: req.Header.Get("User-Agent"),
		Authorization: req.Header.Get("Authorization"), Body: string(body),
	})

	return body
}

// provision is the route the app asks first.
func (a *API) provision(writer http.ResponseWriter, req *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.record(req)

	if a.stalled(req) || a.failed(writer) {
		return
	}

	a.provisioned = true

	answer(writer, `"5.0.21061"`)
}

// guarded wraps a route behind the provisioning, the app's own agent and
// Fail.
func (a *API) guarded(route func(http.ResponseWriter, *http.Request, map[string]string)) http.HandlerFunc {
	return func(writer http.ResponseWriter, req *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()

		var sent map[string]string

		_ = json.Unmarshal(a.record(req), &sent)

		switch {
		case a.stalled(req) || a.failed(writer):
		case !a.provisioned || req.Header.Get("User-Agent") != "android client":
			writer.WriteHeader(http.StatusForbidden)
		default:
			route(writer, req, sent)
		}
	}
}

// stalled holds a request for as long as its client waits, when the API is
// told to stall, and says whether it did. Called with the lock held, which
// it gives up meanwhile.
func (a *API) stalled(req *http.Request) bool {
	if !a.Stall {
		return false
	}

	a.mu.Unlock()
	<-req.Context().Done()
	a.mu.Lock()

	return true
}

// failed answers with the status the API is told to fail with, and says
// whether it did.
func (a *API) failed(writer http.ResponseWriter) bool {
	if a.Fail == 0 {
		return false
	}

	if a.Location != "" {
		writer.Header().Set("Location", a.Location)
	}

	writer.WriteHeader(a.Fail)

	return true
}

func (a *API) status(writer http.ResponseWriter, req *http.Request, sent map[string]string) {
	status := a.Status
	if status == "" {
		status = "ACTIVE"
	}

	if req.URL.Query().Get("country") != Country || sent["email"] != Email {
		status = "UNREGISTERED"
	}

	answer(writer, fmt.Sprintf(`{"accountStatus": %q, "authenticationMethod": "EMAIL_PWD_2FA"}`, status))
}

func (a *API) code(writer http.ResponseWriter, req *http.Request, sent map[string]string) {
	switch {
	case a.TooOften:
		writer.WriteHeader(http.StatusTooManyRequests)
	case req.URL.Query().Get("country") != Country || req.URL.Query().Get("culture") == "" || sent["email"] != Email:
		writer.WriteHeader(http.StatusBadRequest)
	default:
		a.codes++
		a.challenge = fmt.Sprintf("challenge-%d", a.codes)

		answer(writer, fmt.Sprintf(`{"challengeId": %q}`, a.challenge))
	}
}

func (a *API) verify(writer http.ResponseWriter, _ *http.Request, sent map[string]string) {
	if a.challenge == "" || sent["challengeId"] != a.challenge || sent["email"] != Email ||
		sent["password"] != Password || sent["otpCode"] != Code {
		writer.WriteHeader(http.StatusBadRequest)

		return
	}

	// A challenge is answered once.
	a.challenge = ""
	a.token = fmt.Sprintf("session-%d", a.codes)

	answer(writer, fmt.Sprintf(`{"account": "an-account-id", "token": %q, "tokenType": "Bearer"}`, a.token))
}

func (a *API) devices(writer http.ResponseWriter, req *http.Request, _ map[string]string) {
	if a.token == "" || req.Header.Get("Authorization") != "Bearer "+a.token {
		writer.WriteHeader(http.StatusUnauthorized)

		return
	}

	if a.Raw != nil {
		answer(writer, string(a.Raw))

		return
	}

	listed := make([]map[string]any, 0, len(a.Devices))

	for _, device := range a.Devices {
		one := map[string]any{
			"Serial": device.Serial, "Name": device.Name, "ProductType": device.Product, "Version": "21.03.08",
			"AutoUpdate": true, "NewVersionAvailable": false, "LocalCredentials": nil,
		}

		if device.Variant != "" {
			one["variant"] = device.Variant
		}

		switch {
		case device.Sealed != "":
			one["LocalCredentials"] = device.Sealed
		case device.Credential != "":
			one["LocalCredentials"] = Seal(device.Credential)
		}

		listed = append(listed, one)
	}

	raw, err := json.Marshal(listed)
	if err != nil {
		panic(err)
	}

	answer(writer, string(raw))
}

func answer(writer http.ResponseWriter, body string) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(writer, body)
}
