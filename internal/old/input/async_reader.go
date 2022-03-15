package input

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/benthosdev/benthos/v4/internal/component"
	"github.com/benthosdev/benthos/v4/internal/component/input"
	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
	"github.com/benthosdev/benthos/v4/internal/old/input/reader"
	"github.com/benthosdev/benthos/v4/internal/shutdown"
	"github.com/benthosdev/benthos/v4/internal/tracing"
)

//------------------------------------------------------------------------------

// AsyncReader is an input implementation that reads messages from a
// reader.Async component.
type AsyncReader struct {
	connected   int32
	connBackoff backoff.BackOff

	allowSkipAcks bool

	typeStr string
	reader  reader.Async

	stats metrics.Type
	log   log.Modular

	transactions chan message.Transaction
	shutSig      *shutdown.Signaller
}

// NewAsyncReader creates a new AsyncReader input type.
func NewAsyncReader(
	typeStr string,
	allowSkipAcks bool,
	r reader.Async,
	log log.Modular,
	stats metrics.Type,
) (input.Streamed, error) {
	boff := backoff.NewExponentialBackOff()
	boff.InitialInterval = time.Millisecond * 100
	boff.MaxInterval = time.Second
	boff.MaxElapsedTime = 0

	rdr := &AsyncReader{
		connBackoff:   boff,
		allowSkipAcks: allowSkipAcks,
		typeStr:       typeStr,
		reader:        r,
		log:           log,
		stats:         stats,
		transactions:  make(chan message.Transaction),
		shutSig:       shutdown.NewSignaller(),
	}

	go rdr.loop()
	return rdr, nil
}

//------------------------------------------------------------------------------

func (r *AsyncReader) loop() {
	// Metrics paths
	var (
		mRcvd       = r.stats.GetCounter("input_received")
		mConn       = r.stats.GetCounter("input_connection_up")
		mFailedConn = r.stats.GetCounter("input_connection_failed")
		mLostConn   = r.stats.GetCounter("input_connection_lost")
		mLatency    = r.stats.GetTimer("input_latency_ns")
	)

	defer func() {
		r.reader.CloseAsync()
		go func() {
			select {
			case <-r.shutSig.CloseNowChan():
				_ = r.reader.WaitForClose(0)
			case <-r.shutSig.HasClosedChan():
			}
		}()
		_ = r.reader.WaitForClose(shutdown.MaximumShutdownWait())

		atomic.StoreInt32(&r.connected, 0)

		close(r.transactions)
		r.shutSig.ShutdownComplete()
	}()

	pendingAcks := sync.WaitGroup{}
	defer func() {
		r.log.Debugln("Waiting for pending acks to resolve before shutting down.")
		pendingAcks.Wait()
		r.log.Debugln("Pending acks resolved.")
	}()

	initConnection := func() bool {
		initConnCtx, initConnDone := r.shutSig.CloseAtLeisureCtx(context.Background())
		defer initConnDone()
		for {
			if err := r.reader.ConnectWithContext(initConnCtx); err != nil {
				if r.shutSig.ShouldCloseAtLeisure() || err == component.ErrTypeClosed {
					return false
				}
				r.log.Errorf("Failed to connect to %v: %v\n", r.typeStr, err)
				mFailedConn.Incr(1)
				select {
				case <-time.After(r.connBackoff.NextBackOff()):
				case <-initConnCtx.Done():
					return false
				}
			} else {
				r.connBackoff.Reset()
				return true
			}
		}
	}
	if !initConnection() {
		return
	}
	mConn.Incr(1)
	atomic.StoreInt32(&r.connected, 1)

	for {
		readCtx, readDone := r.shutSig.CloseAtLeisureCtx(context.Background())
		msg, ackFn, err := r.reader.ReadWithContext(readCtx)
		readDone()

		// If our reader says it is not connected.
		if err == component.ErrNotConnected {
			mLostConn.Incr(1)
			atomic.StoreInt32(&r.connected, 0)

			// Continue to try to reconnect while still active.
			if !initConnection() {
				return
			}
			mConn.Incr(1)
			atomic.StoreInt32(&r.connected, 1)
		}

		// Close immediately if our reader is closed.
		if r.shutSig.ShouldCloseAtLeisure() || err == component.ErrTypeClosed {
			return
		}

		if err != nil || msg == nil {
			if err != nil && err != component.ErrTimeout && err != component.ErrNotConnected {
				r.log.Errorf("Failed to read message: %v\n", err)
			}
			select {
			case <-time.After(r.connBackoff.NextBackOff()):
			case <-r.shutSig.CloseAtLeisureChan():
				return
			}
			continue
		} else {
			r.connBackoff.Reset()
			mRcvd.Incr(int64(msg.Len()))
			r.log.Tracef("Consumed %v messages from '%v'.\n", msg.Len(), r.typeStr)
		}

		startedAt := time.Now()

		resChan := make(chan error)
		tracing.InitSpans("input_"+r.typeStr, msg)
		select {
		case r.transactions <- message.NewTransaction(msg, resChan):
		case <-r.shutSig.CloseAtLeisureChan():
			return
		}

		pendingAcks.Add(1)
		go func(
			m *message.Batch,
			aFn reader.AsyncAckFn,
			rChan chan error,
		) {
			defer pendingAcks.Done()

			var res error
			var open bool
			select {
			case res, open = <-rChan:
			case <-r.shutSig.CloseNowChan():
				// Even if the pipeline is terminating we still want to attempt
				// to propagate an acknowledgement from in-transit messages.
				return
			}
			if !open {
				return
			}
			mLatency.Timing(time.Since(startedAt).Nanoseconds())
			tracing.FinishSpans(m)

			ackCtx, ackDone := r.shutSig.CloseNowCtx(context.Background())
			if err = aFn(ackCtx, res); err != nil {
				r.log.Errorf("Failed to acknowledge message: %v\n", err)
			}
			ackDone()
		}(msg, ackFn, resChan)
	}
}

// TransactionChan returns a transactions channel for consuming messages from
// this input type.
func (r *AsyncReader) TransactionChan() <-chan message.Transaction {
	return r.transactions
}

// Connected returns a boolean indicating whether this input is currently
// connected to its target.
func (r *AsyncReader) Connected() bool {
	return atomic.LoadInt32(&r.connected) == 1
}

// CloseAsync shuts down the AsyncReader input and stops processing requests.
func (r *AsyncReader) CloseAsync() {
	r.shutSig.CloseAtLeisure()
}

// WaitForClose blocks until the AsyncReader input has closed down.
func (r *AsyncReader) WaitForClose(timeout time.Duration) error {
	go func() {
		<-time.After(timeout - time.Second)
		r.shutSig.CloseNow()
	}()
	select {
	case <-r.shutSig.HasClosedChan():
	case <-time.After(timeout):
		return component.ErrTimeout
	}
	return nil
}

//------------------------------------------------------------------------------