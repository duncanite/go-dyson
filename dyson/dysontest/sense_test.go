package dysontest_test

import (
	"context"
	"testing"
	"time"

	"github.com/duncanite/go-dyson/dyson"
	"github.com/duncanite/go-dyson/dyson/dysontest"
)

// TestSense: a sensor's reading changed while a client is connected is
// told to it, unasked, with every other reading beside; and it is what
// the purifier answers when it is asked afterwards.
func TestSense(t *testing.T) {
	t.Parallel()

	fake := dysontest.New()
	t.Cleanup(fake.Close)

	identity, err := dyson.FromSticker(dysontest.SSID, dysontest.WiFiPassword)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	purifier, err := dyson.Dial(ctx, fake.Addr(), identity)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = purifier.Close() })

	fake.Sense("hact", "0071")

	told, err := purifier.Next(ctx)
	if err != nil || told.Environment == nil || told.Environment.Humidity == nil || *told.Environment.Humidity != 71 ||
		told.Environment.Temperature == nil {
		t.Fatalf("Next after the humidity changed = %+v, %v", told, err)
	}

	fake.Sense("pact", "0004")

	if _, err = purifier.Next(ctx); err != nil {
		t.Fatal(err)
	}

	asked, err := purifier.Environment(ctx)
	if err != nil || asked.Humidity == nil || *asked.Humidity != 71 || asked.Particulates == nil ||
		*asked.Particulates != 4 {
		t.Errorf("Environment after two readings changed = %+v, %v", asked, err)
	}
}
