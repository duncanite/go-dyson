package dyson_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/duncanite/go-dyson/dyson"
)

// reader is a goroutine in Next for as long as its context lasts: the
// states it was told, in order.
type reader struct {
	mu     sync.Mutex
	states []dyson.State
	done   chan struct{}
}

func reading(ctx context.Context, purifier *dyson.Purifier) *reader {
	watching := &reader{done: make(chan struct{})}

	go func() {
		defer close(watching.done)

		for {
			update, err := purifier.Next(ctx)
			if err != nil {
				return
			}

			if update.State != nil {
				watching.mu.Lock()
				watching.states = append(watching.states, *update.State)
				watching.mu.Unlock()
			}
		}
	}()

	return watching
}

func (r *reader) read() []dyson.State {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]dyson.State(nil), r.states...)
}

// await waits for the reader to have been told count states.
func (r *reader) await(t *testing.T, count int) []dyson.State {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for len(r.read()) < count {
		if time.Now().After(deadline) {
			t.Fatalf("the reader was told %d states, want %d: %+v", len(r.read()), count, r.read())
		}

		time.Sleep(2 * time.Millisecond)
	}

	return r.read()
}

// TestASetBesideANext: while one goroutine waits in Next, others set the
// purifier. A Hot+Cool that had said nothing is asked how it stands by
// SetOscillation, and the goroutine in Next is the one that reads the
// answer: the setter is told in the purifier's own words, and the reader
// is told that state and every change after it, none lost and none twice.
func TestASetBesideANext(t *testing.T) {
	t.Parallel()

	fake, purifier := hotCool(t)
	ctx := soon(t)

	watching := reading(ctx, purifier)

	// Time for the reader to be in Next: the test holds either way, and
	// this is the way it is about.
	time.Sleep(50 * time.Millisecond)

	if err := purifier.SetOscillation(ctx, true); err != nil {
		t.Fatalf("SetOscillation beside a Next: %v", err)
	}

	asked := fake.Received()
	if len(asked) != 2 || asked[0].Message != "REQUEST-CURRENT-STATE" || asked[1].Data["oson"] != "OION" ||
		asked[1].Data["osal"] != "0063" {
		t.Fatalf("turning, beside a Next, a purifier that had said nothing sent %+v", asked)
	}

	// Several goroutines set at once.
	var setting sync.WaitGroup

	for _, set := range []func() error{
		func() error { return purifier.SetSpeed(ctx, 7) },
		func() error { return purifier.SetNightMode(ctx, true) },
		func() error { return purifier.SetHeat(ctx, true) },
		func() error { return purifier.SetOscillation(ctx, false) },
		func() error { return purifier.SetSleepTimer(ctx, time.Hour) },
	} {
		setting.Go(func() {
			if err := set(); err != nil {
				t.Errorf("a Set beside a Next: %v", err)
			}
		})
	}

	setting.Wait()

	// The state asked for, the turn, and the five changes.
	watching.await(t, 7)

	// And none twice.
	time.Sleep(50 * time.Millisecond)

	states := watching.read()
	if len(states) != 7 {
		t.Errorf("the reader was told %d states, want 7: %+v", len(states), states)
	}

	if last := states[len(states)-1]; last.Speed != 7 || !last.NightMode || last.Oscillating || last.Heat == nil ||
		!last.Heat.On {
		t.Errorf("the last state the reader was told is %+v", last)
	}

	if sent := fake.Received(); len(sent) != 7 {
		t.Errorf("the purifier was sent %d messages, want the question and six settings: %+v", len(sent), sent)
	}
}

// TestNextFromSeveral: two goroutines in Next share what the purifier
// says, each thing going to one of them. A Next that waits its turn ends
// with its context, and one that ends with its context took nothing: the
// next call is given what it would have been.
func TestNextFromSeveral(t *testing.T) {
	t.Parallel()

	fake, purifier := connected(t)
	ctx := soon(t)

	first, second := reading(ctx, purifier), reading(ctx, purifier)

	const said = 20

	for speed := range said {
		fake.Set("fnsp", []string{"0001", "0002", "0003", "0004"}[speed%4])
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(first.read())+len(second.read()) < said && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	time.Sleep(50 * time.Millisecond)

	if total := len(first.read()) + len(second.read()); total != said {
		t.Errorf("two readers were told %d states between them, want %d", total, said)
	}

	// One goroutine waits in Next; another's wait for its turn ends with
	// its own context, and takes nothing.
	alone, purifier := connected(t)

	held := reading(ctx, purifier)

	time.Sleep(50 * time.Millisecond)

	brief, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	if _, err := purifier.Next(brief); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a Next that waits its turn = %v, want its context's end", err)
	}

	alone.Set("fnsp", "0009")

	if states := held.await(t, 1); states[0].Speed != 9 {
		t.Errorf("the reader in Next was told %+v", states)
	}

	// With nobody in Next: a Next that ends with its context took
	// nothing, and the next is given what came meanwhile.
	quiet, purifier := connected(t)

	brief, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()

	if _, err := purifier.Next(brief); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a Next with nothing said = %v, want its context's end", err)
	}

	quiet.Set("fnsp", "0005")

	if update, err := purifier.Next(ctx); err != nil || update.State == nil || update.State.Speed != 5 {
		t.Errorf("the Next after one that ended = %+v, %v", update, err)
	}
}
