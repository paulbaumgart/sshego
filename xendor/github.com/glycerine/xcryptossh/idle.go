package ssh

import (
	"sync"
	"sync/atomic"
	"time"
)

// with	import "runtime/debug"
//func init() {
//  // see all goroutines on panic for proper debugging.
//	debug.SetTraceback("all")
//}

// idleTimer allows a client of the ssh
// library to notice if there has been a
// stall in i/o activity. This enables
// clients to impliment timeout logic
// that works and doesn't timeout under
// long-duration-but-still-successful
// reads/writes.
//
// It is probably simpler to use the
// SetIdleTimeout(dur time.Duration)
// method on the channel.
//
type idleTimer struct {
	mut             sync.Mutex
	idleDur         time.Duration
	last            uint64
	halt            *Halter
	timeoutCallback func()

	// GetIdleTimeoutCh returns the current idle timeout duration in use.
	// It will return 0 if timeouts are disabled.
	getIdleTimeoutCh chan time.Duration
	setIdleTimeoutCh chan *setTimeoutTicket
	TimedOut         chan bool

	setCallback   chan *callbacks
	timeOutRaised bool
}

type callbacks struct {
	onTimeout func()
}

// newIdleTimer creates a new idleTimer which will call
// the `callback` function provided after `dur` inactivity.
// If callback is nil, you must use setTimeoutCallback()
// to establish the callback before activating the timer
// with SetIdleTimeout. The `dur` can be 0 to begin with no
// timeout, in which case the timer will be inactive until
// SetIdleTimeout is called.
func newIdleTimer(callback func(), dur time.Duration) *idleTimer {
	t := &idleTimer{
		getIdleTimeoutCh: make(chan time.Duration),
		setIdleTimeoutCh: make(chan *setTimeoutTicket),
		setCallback:      make(chan *callbacks),
		TimedOut:         make(chan bool),
		halt:             NewHalter(),
		timeoutCallback:  callback,
	}
	go t.backgroundStart(dur)
	return t
}

func (t *idleTimer) setTimeoutCallback(timeoutFunc func()) {
	select {
	case t.setCallback <- &callbacks{onTimeout: timeoutFunc}:
	case <-t.halt.ReqStop.Chan:
	}
}

// Reset stores the current monotonic timestamp
// internally, effectively reseting to zero the value
// returned from an immediate next call to NanosecSince().
//
func (t *idleTimer) Reset() {
	atomic.StoreUint64(&t.last, monoNow())
}

// NanosecSince returns how many nanoseconds it has
// been since the last call to Reset().
func (t *idleTimer) NanosecSince() uint64 {
	return monoNow() - atomic.LoadUint64(&t.last)
}

// SetIdleTimeout stores a new idle timeout duration. This
// activates the idleTimer if dur > 0. Set dur of 0
// to disable the idleTimer. A disabled idleTimer
// always returns false from TimedOut().
//
// This is the main API for idleTimer. Most users will
// only need to use this call.
//
func (t *idleTimer) SetIdleTimeout(dur time.Duration) {
	tk := newSetTimeoutTicket(dur)
	select {
	case t.setIdleTimeoutCh <- tk:
	case <-t.halt.ReqStop.Chan:
	}
	select {
	case <-tk.done:
	case <-t.halt.ReqStop.Chan:
	}

}

// GetIdleTimeout returns the current idle timeout duration in use.
// It will return 0 if timeouts are disabled.
func (t *idleTimer) GetIdleTimeout() (dur time.Duration) {
	select {
	case dur = <-t.getIdleTimeoutCh:
	case <-t.halt.ReqStop.Chan:
	}
	return
}

func (t *idleTimer) Stop() {
	t.halt.ReqStop.Close()
	select {
	case <-t.halt.Done.Chan:
	case <-time.After(10 * time.Second):
		panic("idleTimer.Stop() problem! t.halt.Done.Chan not received  after 10sec! serious problem")
	}
}

type setTimeoutTicket struct {
	newdur time.Duration
	done   chan struct{}
}

func newSetTimeoutTicket(dur time.Duration) *setTimeoutTicket {
	return &setTimeoutTicket{
		newdur: dur,
		done:   make(chan struct{}),
	}
}

func (t *idleTimer) backgroundStart(dur time.Duration) {
	go func() {
		var heartbeat *time.Ticker
		var heartch <-chan time.Time
		if dur > 0 {
			heartbeat = time.NewTicker(dur)
			heartch = heartbeat.C
		}
		defer func() {
			if heartbeat != nil {
				heartbeat.Stop() // allow GC
			}
			t.halt.Done.Close()
		}()
		for {
			select {
			case <-t.halt.ReqStop.Chan:
				return

			case t.TimedOut <- t.timeOutRaised:
				continue

			case f := <-t.setCallback:
				t.timeoutCallback = f.onTimeout

			case t.getIdleTimeoutCh <- dur:
				continue

			case tk := <-t.setIdleTimeoutCh:
				if dur > 0 {
					// timeouts active currently
					if tk.newdur == dur {
						close(tk.done)
						continue
					}
					if tk.newdur <= 0 {
						// stopping timeouts
						if heartbeat != nil {
							heartbeat.Stop() // allow GC
						}
						dur = tk.newdur
						heartbeat = nil
						heartch = nil
						/* change state, maybe */
						t.timeOutRaised = false
						close(tk.done)
						continue
					}
					// changing an active timeout dur
					if heartbeat != nil {
						heartbeat.Stop() // allow GC
					}
					dur = tk.newdur
					heartbeat = time.NewTicker(dur)
					heartch = heartbeat.C
					t.Reset()
					close(tk.done)
					continue
				} else {
					// heartbeats not currently active
					if tk.newdur <= 0 {
						dur = 0
						// staying inactive
						close(tk.done)
						continue
					}
					// heartbeats activating
					t.timeOutRaised = false
					dur = tk.newdur
					heartbeat = time.NewTicker(dur)
					heartch = heartbeat.C
					t.Reset()
					close(tk.done)
					continue
				}

			case <-heartch:
				if dur == 0 {
					panic("should be impossible to get heartbeat.C on dur == 0")
				}
				if t.NanosecSince() > uint64(dur) {
					/* change state */
					t.timeOutRaised = true

					// After firing, disable until reactivated.
					// Still must be a ticker and not a one-shot because it may take
					// many, many heartbeats before a timeout, if one happens
					// at all.
					if heartbeat != nil {
						heartbeat.Stop() // allow GC
					}
					heartbeat = nil
					heartch = nil
					if t.timeoutCallback == nil {
						panic("idleTimer.timeoutCallback was never set! call t.setTimeoutCallback()!!!")
					}
					// our caller may be holding locks...
					// and timeoutCallback will want locks...
					// so unless we start timeoutCallback() on its
					// own goroutine, we are likely to deadlock.
					go t.timeoutCallback()
				}
			}
		}
	}()
}