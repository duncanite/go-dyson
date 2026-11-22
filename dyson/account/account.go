// Package account fetches from a Dyson account what a purifier's Wi-Fi
// sticker would have given: its serial, its product type and its
// credential. A purifier that carries no sticker has no other source.
// It is asked once: the purifier is then spoken to on the LAN as any
// other, with no account.
//
// Dyson publishes no API. This is the one its own app uses, as the open
// implementations call it: a login by the account's address and password
// with a one-time code Dyson sends by mail, then the list of the
// account's devices. It cannot run unattended, and Dyson can change it.
//
// Nothing is written anywhere: where an identity is kept is the program's
// business.
package account

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/duncanite/go-dyson/dyson"
)

// BaseURL is where the API is, for every country but mainland China.
const BaseURL = "https://appapi.cp.dyson.com"

const (
	// maxAnswer bounds one answer read; a household's devices is a few
	// kilobytes.
	maxAnswer = 4 << 20
	// agent is what the API takes requests from.
	agent = "android client"

	pathProvision = "/v1/provisioningservice/application/Android/version"
	pathStatus    = "/v3/userregistration/email/userstatus"
	pathCode      = "/v3/userregistration/email/auth"
	pathVerify    = "/v3/userregistration/email/verify"
	pathDevices   = "/v2/provisioningservice/manifest"
)

// The errors a Client hands back, to test with errors.Is. None carries
// the address, the password, the code or a credential.
var (
	// ErrCredentials: Dyson refused the password or the code, or takes
	// the session no more.
	ErrCredentials = errors.New("dyson refused the password, the code or the session")
	// ErrTooOften: codes were asked for too often; Dyson answered 429.
	ErrTooOften = errors.New("dyson was asked for a code too often")
	// ErrAccount: the address is no active account's. The error carries
	// what Dyson called it.
	ErrAccount = errors.New("not an active dyson account")
	// ErrRefused: any other answer that is not a success.
	ErrRefused = errors.New("dyson refused the request")
	// ErrAnswer: an answer is not in the shape the open implementations
	// read, or is larger than an answer may be.
	ErrAnswer = errors.New("dyson's answer is not in the expected shape")
	// ErrUnread: a device of the list could not be read; Devices gives
	// the others with it.
	ErrUnread = errors.New("a device of the account could not be read")
	// ErrArgument: something that cannot be sent; nothing was.
	ErrArgument = errors.New("not a valid argument")
)

// Doer makes one HTTP request; an *http.Client is one.
type Doer interface {
	// Do sends the request and returns the answer, as http.Client.Do does.
	Do(request *http.Request) (*http.Response, error)
}

// Options shape a Client.
type Options struct {
	// BaseURL is where the API is; empty is BaseURL.
	BaseURL string
	// HTTP is what every request goes through; nil is a client that
	// follows no redirect. A redirect followed would carry the login's
	// body, the password in it, to wherever it points: a Doer given is to
	// follow none either.
	HTTP Doer
	// Country is the account's country, as two capital letters: "US",
	// "GB", "FR". The API asks for it.
	Country string
}

// Client is the API for the accounts of one country. Its methods may be
// called from several goroutines.
type Client struct {
	base    string
	http    Doer
	country string
}

// New is a client. Nothing is sent until a method is called.
func New(opts Options) *Client {
	client := &Client{base: strings.TrimSuffix(opts.BaseURL, "/"), http: opts.HTTP, country: opts.Country}

	if client.base == "" {
		client.base = BaseURL
	}

	if client.http == nil {
		client.http = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}

	return client
}

// Challenge is a code asked for: Dyson has sent it to the account's
// address, and Answer takes it. Formatted or logged, it shows neither the
// address nor what Dyson named it by.
type Challenge struct {
	email string
	id    string
}

// Format keeps the challenge out of a formatted value, whatever the verb.
func (Challenge) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "dyson account challenge")
}

// LogValue keeps it out of a log record.
func (Challenge) LogValue() slog.Value { return slog.StringValue("dyson account challenge") }

// Session is an account logged in to. It is a secret: formatted or
// logged, it shows nothing of itself.
type Session struct {
	token string
}

// Format keeps the session out of a formatted value, whatever the verb.
func (Session) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, "dyson account session") }

// LogValue keeps it out of a log record.
func (Session) LogValue() slog.Value { return slog.StringValue("dyson account session") }

// Device is one device of an account that can be spoken to on the LAN.
type Device struct {
	// Name is what its owner called it.
	Name string
	// Identity is what opens it: what dyson.Dial takes.
	Identity dyson.Identity
}

var country = regexp.MustCompile(`^[A-Z]{2}$`)

// Ask has Dyson send a one-time code to the account's address, and
// returns what Answer needs with it. Every call sends a code, and Dyson
// refuses to send them too often: ErrTooOften. An address that is no
// active account's is ErrAccount.
func (c *Client) Ask(ctx context.Context, email string) (Challenge, error) {
	if !country.MatchString(c.country) {
		return Challenge{}, fmt.Errorf("%w: a country is two capital letters", ErrArgument)
	}

	if email == "" {
		return Challenge{}, fmt.Errorf("%w: an empty address", ErrArgument)
	}

	if err := c.provision(ctx); err != nil {
		return Challenge{}, err
	}

	var account struct {
		Status string `json:"accountStatus"` //nolint:tagliatelle // the API's own key
	}

	query := url.Values{"country": {c.country}}
	address := map[string]string{"email": email}

	status, err := c.exchange(ctx, http.MethodPost, pathStatus, query, address, "", &account)
	if err != nil {
		return Challenge{}, err
	}

	if status != http.StatusOK {
		return Challenge{}, refusal(pathStatus, status)
	}

	if account.Status != "ACTIVE" {
		return Challenge{}, fmt.Errorf("%w: Dyson calls it %q", ErrAccount, account.Status)
	}

	var asked struct {
		ID string `json:"challengeId"` //nolint:tagliatelle // the API's own key
	}

	query.Set("culture", "en-US")

	status, err = c.exchange(ctx, http.MethodPost, pathCode, query, address, "", &asked)
	if err != nil {
		return Challenge{}, err
	}

	switch {
	case status == http.StatusTooManyRequests:
		return Challenge{}, fmt.Errorf("%w: POST %s: HTTP %d", ErrTooOften, pathCode, status)
	case status != http.StatusOK:
		return Challenge{}, refusal(pathCode, status)
	case asked.ID == "":
		return Challenge{}, fmt.Errorf("%w: POST %s gave no challenge", ErrAnswer, pathCode)
	}

	return Challenge{email: email, id: asked.ID}, nil
}

// Answer logs in to the account with its password and the code Dyson
// sent for the challenge. A password or a code Dyson refuses is
// ErrCredentials.
func (c *Client) Answer(ctx context.Context, challenge Challenge, password, code string) (Session, error) {
	if challenge.id == "" || password == "" || code == "" {
		return Session{}, fmt.Errorf("%w: a challenge, a password and a code are all needed", ErrArgument)
	}

	var granted struct {
		Token string `json:"token"`
		Kind  string `json:"tokenType"` //nolint:tagliatelle // the API's own key
	}

	status, err := c.exchange(ctx, http.MethodPost, pathVerify, nil, map[string]string{
		"email": challenge.email, "password": password, "challengeId": challenge.id, "otpCode": code,
	}, "", &granted)
	if err != nil {
		return Session{}, err
	}

	switch {
	case status == http.StatusBadRequest:
		return Session{}, fmt.Errorf("%w: POST %s: HTTP %d", ErrCredentials, pathVerify, status)
	case status != http.StatusOK:
		return Session{}, refusal(pathVerify, status)
	case granted.Token == "" || granted.Kind != "Bearer":
		return Session{}, fmt.Errorf("%w: POST %s gave no bearer token", ErrAnswer, pathVerify)
	}

	return Session{token: granted.Token}, nil
}

// record is one device as the list gives it.
type record struct {
	Serial      string  `json:"Serial"`           //nolint:tagliatelle // the API's own key
	Name        string  `json:"Name"`             //nolint:tagliatelle // the API's own key
	Product     string  `json:"ProductType"`      //nolint:tagliatelle // the API's own key
	Variant     string  `json:"variant"`          // the API's own key
	Credentials *string `json:"LocalCredentials"` //nolint:tagliatelle // the API's own key
}

// Devices is the account's devices that have a credential for the LAN, in
// the order Dyson lists them. One that has none, a light, is left out.
//
// A device whose credential does not decrypt, or that has no serial or no
// product type, cannot be read: it is left out too, and the error is then
// ErrUnread, with the devices that could be read returned beside it. One
// device Dyson writes otherwise does not hide the others.
//
// A purifier's product type is the list's, with its variant's letter
// after it for the generations that have variants. The open
// implementations also read a variant out of the firmware's version when
// the list gives none; this does not.
func (c *Client) Devices(ctx context.Context, session Session) ([]Device, error) {
	if session.token == "" {
		return nil, fmt.Errorf("%w: no session", ErrArgument)
	}

	if err := c.provision(ctx); err != nil {
		return nil, err
	}

	var listed []record

	status, err := c.exchange(ctx, http.MethodGet, pathDevices, nil, nil, session.token, &listed)
	if err != nil {
		return nil, err
	}

	if status != http.StatusOK {
		return nil, refusal(pathDevices, status)
	}

	devices := make([]Device, 0, len(listed))
	credentialed, unread := 0, 0

	for _, one := range listed {
		if one.Credentials == nil {
			continue
		}

		credentialed++

		credential, opened := decrypted(*one.Credentials)
		if !opened || one.Serial == "" || one.Product == "" {
			unread++

			continue
		}

		devices = append(devices, Device{
			Name: one.Name,
			Identity: dyson.Identity{
				Serial:     one.Serial,
				Type:       productType(one.Product, one.Variant),
				Credential: credential,
			},
		})
	}

	if unread > 0 {
		return devices, fmt.Errorf("%w: %d of the %d that have a credential", ErrUnread, unread, credentialed)
	}

	return devices, nil
}

// productType is the first level of a device's topics: its product type,
// and for the generations that have variants the variant's letter.
func productType(product, variant string) string {
	variant = strings.ToUpper(strings.TrimSpace(variant))

	switch product {
	case "438", "527", "358":
		if variant == "E" || variant == "K" || variant == "M" {
			return product + variant
		}
	}

	return product
}

// decrypted is the broker's password out of a device's local credential:
// base64 of AES-256 in CBC mode, under the key and the vector every
// device's is encrypted with, of JSON that holds it. What it decrypted to
// is never said, not in an error either: it is, or is near, a secret.
func decrypted(sealed string) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return "", false
	}

	// The key is the bytes 1 to 32, and the vector is zeros: the same for
	// every device, and no secret.
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", false
	}

	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(raw, raw)

	// A padding of nothing leaves the whole of it to be read as JSON, which
	// it then is not.
	padding := int(raw[len(raw)-1])
	if padding > aes.BlockSize ||
		!bytes.Equal(raw[len(raw)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
		return "", false
	}

	var held struct {
		Password string `json:"apPasswordHash"` //nolint:tagliatelle // the API's own key
	}

	if json.Unmarshal(raw[:len(raw)-padding], &held) != nil || held.Password == "" {
		return "", false
	}

	return held.Password, true
}

// provision asks the route the app asks first: the API takes the others
// from a client that has.
func (c *Client) provision(ctx context.Context) error {
	status, err := c.exchange(ctx, http.MethodGet, pathProvision, nil, nil, "", nil)
	if err != nil {
		return err
	}

	if status != http.StatusOK {
		return refusal(pathProvision, status)
	}

	return nil
}

// refusal is the error for a status no route expects.
func refusal(path string, status int) error {
	return fmt.Errorf("%w: %s: HTTP %d", ErrRefused, path, status)
}

// exchange sends one request and returns the status it was answered
// with; a success is decoded into the value given, when one is. A 401 or
// a 403 is ErrCredentials, whatever the route.
func (c *Client) exchange(
	ctx context.Context,
	method, path string,
	query url.Values,
	body any,
	token string,
	into any,
) (int, error) {
	request, err := c.request(ctx, method, path, query, body, token)
	if err != nil {
		return 0, err
	}

	response, err := c.http.Do(request)
	if err != nil {
		// The client's error names the URL, whose query holds the country
		// and nothing else of the account.
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}

	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxAnswer+1))
	if err != nil {
		return 0, fmt.Errorf("%s %s: reading the answer: %w", method, path, err)
	}

	if len(raw) > maxAnswer {
		return 0, fmt.Errorf("%w: %s %s: over %d bytes", ErrAnswer, method, path, maxAnswer)
	}

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return response.StatusCode, fmt.Errorf("%w: %s %s: HTTP %d", ErrCredentials, method, path, response.StatusCode)
	}

	if response.StatusCode == http.StatusOK && into != nil {
		if err = json.Unmarshal(raw, into); err != nil {
			// The decoder's error may quote the answer, which may hold a
			// token or a credential: it is not handed on.
			return response.StatusCode, fmt.Errorf("%w: %s %s is not the JSON expected", ErrAnswer, method, path)
		}
	}

	return response.StatusCode, nil
}

// request is one request as the API takes it: the app's agent, JSON both
// ways, the token when there is one.
func (c *Client) request(
	ctx context.Context,
	method, path string,
	query url.Values,
	body any,
	token string,
) (*http.Request, error) {
	var payload io.Reader = http.NoBody

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%w: the body for %s: %w", ErrArgument, path, err)
		}

		payload = bytes.NewReader(raw)
	}

	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	request, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %w", ErrArgument, method, path, err)
	}

	request.Header.Set("User-Agent", agent)
	request.Header.Set("Accept", "application/json")

	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	return request, nil
}
