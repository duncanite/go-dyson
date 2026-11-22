package account_test

// The account API as the open implementations call it, read off
// libdyson-neon's account client and its decryption:
//
//   - GET /v1/provisioningservice/application/Android/version comes
//     first: the API takes the other routes from a client that asked it.
//   - POST /v3/userregistration/email/userstatus?country=CC takes the
//     address and says whether the account is ACTIVE.
//   - POST /v3/userregistration/email/auth?country=CC&culture=en-US takes
//     the address, sends a code by mail and gives a challengeId; 429 when
//     asked too often.
//   - POST /v3/userregistration/email/verify takes the address, the
//     password, the challengeId and the otpCode, and gives a bearer
//     token; 400 for a password or a code it refuses.
//   - GET /v2/provisioningservice/manifest, under the token, lists the
//     devices: Serial, Name, ProductType, variant, and LocalCredentials,
//     which is base64 of AES-256-CBC under the bytes 1 to 32 and a zero
//     vector, of JSON whose apPasswordHash is the broker's password.

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/duncanite/go-dyson/dyson"
	"github.com/duncanite/go-dyson/dyson/account"
	"github.com/duncanite/go-dyson/dyson/account/accounttest"
)

func started(t *testing.T) (*accounttest.API, *account.Client) {
	t.Helper()

	api := accounttest.New()
	t.Cleanup(api.Close)

	return api, account.New(account.Options{BaseURL: api.URL, HTTP: api.Client(), Country: accounttest.Country})
}

// loggedIn is a session on the fake's account.
func loggedIn(t *testing.T, client *account.Client) account.Session {
	t.Helper()

	challenge, err := client.Ask(t.Context(), accounttest.Email)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	session, err := client.Answer(t.Context(), challenge, accounttest.Password, accounttest.Code)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	return session
}

// TestLogin: the requests of a login and of the list, in order and as the
// API takes them: the provisioning first, the app's agent on every one,
// the country where it is asked for, one code sent, the token on the list
// and nowhere before.
func TestLogin(t *testing.T) {
	t.Parallel()

	api, client := started(t)
	session := loggedIn(t, client)

	if _, err := client.Devices(t.Context(), session); err != nil {
		t.Fatalf("Devices: %v", err)
	}

	requests, codes := api.Received()

	want := []accounttest.Request{
		{Method: "GET", Path: "/v1/provisioningservice/application/Android/version"},
		{
			Method: "POST", Path: "/v3/userregistration/email/userstatus", Query: "country=GB",
			Body: `{"email":"owner@example.test"}`,
		},
		{
			Method: "POST", Path: "/v3/userregistration/email/auth", Query: "country=GB&culture=en-US",
			Body: `{"email":"owner@example.test"}`,
		},
		{
			Method: "POST",
			Path:   "/v3/userregistration/email/verify",
			Body:   `{"challengeId":"challenge-1","email":"owner@example.test","otpCode":"123456","password":"account-pass"}`,
		},
		{Method: "GET", Path: "/v1/provisioningservice/application/Android/version"},
		{Method: "GET", Path: "/v2/provisioningservice/manifest", Authorization: "Bearer session-1"},
	}

	for index := range want {
		want[index].Agent = "android client"
	}

	if codes != 1 || !reflect.DeepEqual(requests, want) {
		t.Errorf("%d codes sent, and the requests were\n%+v\nwant one code and\n%+v", codes, requests, want)
	}
}

// TestDevices: each device of the account with a credential for the LAN
// is an identity Dial takes, in the list's order: its serial, its type
// with the variant's letter for the generations that have variants, its
// credential decrypted. A device with no credential is left out. A
// credential encrypted by another implementation under the same key reads
// the same.
func TestDevices(t *testing.T) {
	t.Parallel()

	api, client := started(t)

	// Encrypted with OpenSSL: aes-256-cbc under the bytes 1 to 32 and a
	// zero vector, of {"serial":…,"apPasswordHash":…}.
	const (
		sealed = "lh5FJ65cd0JCGZkd0mbmyovzOA0/G+qoQbEAehaSEpdYMcU3oDdPwd7rV8tF7l8zDYwy4vrEdGZmljaF/T4F5le1aLXAtF/vNYL6Od4l" +
			"dUPE/zR4qPP9W03sYoENxUxWBMQlUl5GMa28YiUrTtsqcg=="
		opened = "dGhlIGJyb2tlcidzIHBhc3N3b3JkLCBub3QgYSByZWFsIG9uZQ=="
	)

	api.Devices = []accounttest.Device{
		{Serial: "AB1-EU-CDE2345F", Name: "Bedroom", Product: "475", Sealed: sealed},
		{Serial: "AB2-EU-CDE2345F", Name: "A light", Product: "552"},
		{Serial: "AB3-EU-CDE2345F", Name: "Living room", Product: "527", Credential: "second"},
		{Serial: "AB4-EU-CDE2345F", Name: "Office", Product: "527", Variant: " k ", Credential: "third"},
		{Serial: "AB5-EU-CDE2345F", Name: "Hall", Product: "438", Variant: "X", Credential: "fourth"},
		{Serial: "AB6-EU-CDE2345F", Name: "Attic", Product: "475", Variant: "E", Credential: "fifth"},
	}

	devices, err := client.Devices(t.Context(), loggedIn(t, client))
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}

	want := []account.Device{
		{Name: "Bedroom", Identity: dyson.Identity{Serial: "AB1-EU-CDE2345F", Type: "475", Credential: opened}},
		{Name: "Living room", Identity: dyson.Identity{Serial: "AB3-EU-CDE2345F", Type: "527", Credential: "second"}},
		{Name: "Office", Identity: dyson.Identity{Serial: "AB4-EU-CDE2345F", Type: "527K", Credential: "third"}},
		{Name: "Hall", Identity: dyson.Identity{Serial: "AB5-EU-CDE2345F", Type: "438", Credential: "fourth"}},
		{Name: "Attic", Identity: dyson.Identity{Serial: "AB6-EU-CDE2345F", Type: "475", Credential: "fifth"}},
	}

	if !reflect.DeepEqual(devices, want) {
		for _, device := range devices {
			t.Logf("%s: %s %s, credential %q", device.Name, device.Identity.Serial, device.Identity.Type,
				device.Identity.Credential)
		}

		t.Errorf("Devices gave %d devices, not the %d wanted, or not as wanted", len(devices), len(want))
	}
}

// TestRefused: what Dyson refuses, each as its own error, and none
// carrying the address, the password, the code, a token or a credential.
func TestRefused(t *testing.T) {
	t.Parallel()

	secrets := []string{accounttest.Email, accounttest.Password, accounttest.Code, "session-", "hidden-credential"}

	check := func(name string, err, want error, saying string) {
		t.Helper()

		if !errors.Is(err, want) || !strings.Contains(err.Error(), saying) {
			t.Errorf("%s = %v, want %v saying %q", name, err, want, saying)

			return
		}

		for _, secret := range secrets {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error carries %q: %v", name, secret, err)
			}
		}
	}

	api, client := started(t)
	ctx := t.Context()

	challenge, err := client.Ask(ctx, accounttest.Email)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	_, err = client.Answer(ctx, challenge, "another password", accounttest.Code)
	check("a password Dyson refuses", err, account.ErrCredentials, "HTTP 400")

	_, err = client.Answer(ctx, challenge, accounttest.Password, "000000")
	check("a code Dyson refuses", err, account.ErrCredentials, "HTTP 400")

	first, err := client.Answer(ctx, challenge, accounttest.Password, accounttest.Code)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}

	_, err = client.Answer(ctx, challenge, accounttest.Password, accounttest.Code)
	check("a challenge answered twice", err, account.ErrCredentials, "HTTP 400")

	// Another login: the first session is taken no more.
	second := loggedIn(t, client)

	_, err = client.Devices(ctx, first)
	check("a session Dyson no longer takes", err, account.ErrCredentials, "HTTP 401")

	_, err = client.Ask(ctx, "someone@example.test")
	check("an address that is no account's", err, account.ErrAccount, "UNREGISTERED")

	api.TooOften = true

	_, err = client.Ask(ctx, accounttest.Email)
	check("a code asked for too often", err, account.ErrTooOften, "HTTP 429")

	api.TooOften = false

	api.Raw = []byte(`[{"Serial": "AB1-EU-CDE2345F", "LocalCredentials": "hidden-credential", `)

	_, err = client.Devices(ctx, second)
	check("a list that is no JSON", err, account.ErrAnswer, "not the JSON expected")

	api.Raw, api.Fail = nil, 503

	_, err = client.Devices(ctx, second)
	check("a route that fails", err, account.ErrRefused, "HTTP 503")

	_, err = client.Ask(ctx, accounttest.Email)
	check("the first route failing", err, account.ErrRefused, "HTTP 503")
}

// TestUnread: a device whose credential does not decrypt, or that has no
// serial or no type, is left out and said: the error is ErrUnread, and the
// devices that could be read come with it. No credential, however it is
// malformed, is a panic, and none is shown.
func TestUnread(t *testing.T) {
	t.Parallel()

	api, client := started(t)
	session := loggedIn(t, client)

	whole := func(plain string) string { return accounttest.SealBlocks([]byte(plain)) }

	unreadable := map[string]accounttest.Device{
		"a credential that is no base64":   {Sealed: "not base64!"},
		"a credential of no whole block":   {Sealed: "aGlkZGVuLWNyZWRlbnRpYWw="},
		"a credential under another key":   {Sealed: "aGlkZGVuLWNyZWRlbnRpYWwhaGlkZGVuLWNyZWRlbnQ="},
		"a credential with no password":    {Sealed: accounttest.Seal("")},
		"a credential that is no JSON":     {Sealed: whole("hidden-credential" + strings.Repeat("\x0f", 15))},
		"a padding of nothing":             {Sealed: whole(`{"apPasswordHash":"x"}` + strings.Repeat("\x00", 10))},
		"a padding longer than a block":    {Sealed: whole(`{"apPasswordHash":"x"}` + strings.Repeat(" ", 10))},
		"a padding longer than it all":     {Sealed: whole("0123456789abcde\xc8")},
		"a padding that is not all itself": {Sealed: whole(`{"apPasswordHash":"x"}` + "         \n")},
	}

	for name, device := range unreadable {
		device.Serial, device.Product = "AB1-EU-CDE2345F", "475"
		api.Devices = []accounttest.Device{device}

		devices, err := client.Devices(t.Context(), session)
		if !errors.Is(err, account.ErrUnread) || len(devices) != 0 || strings.Contains(err.Error(), "hidden") {
			t.Errorf("%s = %+v, %v, want ErrUnread and no device", name, devices, err)
		}
	}

	// As the list may write them: an empty credential, a device with no
	// serial, one with no type, between two that read.
	api.Devices = nil
	api.Raw = []byte(`[
		{"Serial": "AB1-EU-CDE2345F", "Name": "First", "ProductType": "475", "LocalCredentials": "` +
		accounttest.Seal("first") + `"},
		{"Serial": "AB2-EU-CDE2345F", "Name": "Empty", "ProductType": "475", "LocalCredentials": ""},
		{"Name": "Nameless", "ProductType": "475", "LocalCredentials": "` + accounttest.Seal("second") + `"},
		{"Serial": "AB4-EU-CDE2345F", "Name": "Typeless", "LocalCredentials": "` + accounttest.Seal("third") + `"},
		{"Serial": "AB5-EU-CDE2345F", "Name": "A light", "ProductType": "552", "LocalCredentials": null},
		{"Serial": "AB6-EU-CDE2345F", "Name": "Last", "ProductType": "527", "LocalCredentials": "` +
		accounttest.Seal("fourth") + `"}
	]`)

	devices, err := client.Devices(t.Context(), session)

	want := []account.Device{
		{Name: "First", Identity: dyson.Identity{Serial: "AB1-EU-CDE2345F", Type: "475", Credential: "first"}},
		{Name: "Last", Identity: dyson.Identity{Serial: "AB6-EU-CDE2345F", Type: "527", Credential: "fourth"}},
	}

	if !errors.Is(err, account.ErrUnread) || !strings.Contains(err.Error(), "3 of the 5") ||
		!reflect.DeepEqual(devices, want) {
		t.Errorf(
			"Devices of a list with three that do not read = %d devices, %v\nwant the two that read, and ErrUnread for 3 of the 5",
			len(devices),
			err,
		)
	}
}

// TestNoRedirect: a client given nothing to send its requests through
// follows no redirect: the login's body goes nowhere but where it was
// sent.
func TestNoRedirect(t *testing.T) {
	t.Parallel()

	api := accounttest.New()
	t.Cleanup(api.Close)

	elsewhere := accounttest.New()
	t.Cleanup(elsewhere.Close)

	api.Fail, api.Location = 307, elsewhere.URL+"/v1/provisioningservice/application/Android/version"

	client := account.New(account.Options{BaseURL: api.URL, Country: accounttest.Country})

	if _, err := client.Ask(t.Context(), accounttest.Email); !errors.Is(err, account.ErrRefused) ||
		!strings.Contains(err.Error(), "HTTP 307") {
		t.Errorf("Ask of an API that redirects = %v, want ErrRefused for the 307", err)
	}

	if followed, _ := elsewhere.Received(); len(followed) != 0 {
		t.Errorf("the redirect was followed: %+v", followed)
	}
}

// TestArguments: what cannot be sent is ErrArgument, and nothing is sent:
// a country that is not two capital letters, an empty address, an answer
// with no challenge, no password or no code, a list with no session.
func TestArguments(t *testing.T) {
	t.Parallel()

	api, client := started(t)
	ctx := t.Context()

	for _, written := range []string{"", "gb", "GBR", "G1"} {
		abroad := account.New(account.Options{BaseURL: api.URL, HTTP: api.Client(), Country: written})
		if _, err := abroad.Ask(ctx, accounttest.Email); !errors.Is(err, account.ErrArgument) {
			t.Errorf("Ask under the country %q = %v, want ErrArgument", written, err)
		}
	}

	if _, err := client.Ask(ctx, ""); !errors.Is(err, account.ErrArgument) {
		t.Errorf("Ask with no address = %v, want ErrArgument", err)
	}

	if _, err := client.Devices(ctx, account.Session{}); !errors.Is(err, account.ErrArgument) {
		t.Errorf("Devices with no session = %v, want ErrArgument", err)
	}

	if _, err := client.Answer(ctx, account.Challenge{}, accounttest.Password, accounttest.Code); !errors.Is(
		err, account.ErrArgument,
	) {
		t.Errorf("Answer with no challenge = %v, want ErrArgument", err)
	}

	if sent, _ := api.Received(); len(sent) != 0 {
		t.Fatalf("what could not be sent was: %+v", sent)
	}

	challenge, err := client.Ask(ctx, accounttest.Email)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	before, _ := api.Received()

	for _, pair := range [][2]string{{"", accounttest.Code}, {accounttest.Password, ""}} {
		if _, err = client.Answer(ctx, challenge, pair[0], pair[1]); !errors.Is(err, account.ErrArgument) {
			t.Errorf("Answer with a password or a code missing = %v, want ErrArgument", err)
		}
	}

	if after, _ := api.Received(); len(after) != len(before) {
		t.Errorf("an answer that could not be sent was: %+v", after[len(before):])
	}
}

// TestASessionDoesNotPrint: a session shows nothing of itself however it
// is printed or logged, and neither does a challenge: not the address, nor
// what Dyson named it by.
func TestASessionDoesNotPrint(t *testing.T) {
	t.Parallel()

	_, client := started(t)

	challenge, err := client.Ask(t.Context(), accounttest.Email)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		printed := fmt.Sprintf(verb, challenge) + fmt.Sprintf(verb, &challenge) + challenge.LogValue().String()
		if strings.Contains(printed, "example.test") || strings.Contains(printed, "challenge-1") ||
			strings.Contains(printed, "6578616d706c65") {
			t.Errorf("a Challenge under %s shows itself: %s", verb, printed)
		}
	}

	session := loggedIn(t, client)

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		printed := fmt.Sprintf(verb, session) + fmt.Sprintf(verb, &session) + session.LogValue().String()
		if strings.Contains(printed, "session-") || strings.Contains(printed, "73657373696f6e") {
			t.Errorf("a Session under %s shows itself: %s", verb, printed)
		}
	}
}
