package zipper

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/yomorun/yomo/core/quic"
	"github.com/yomorun/yomo/internal/core"
	"github.com/yomorun/yomo/internal/frame"
	"github.com/yomorun/yomo/logger"
	"github.com/yomorun/yomo/zipper/tracing"
)

// DispatcherWithFunc dispatches the input stream to downstreams.
func DispatcherWithFunc(ctx context.Context, sfns []GetStreamFunc, stream quic.Stream) chan *frame.DataFrame {
	next := readDataFromSource(ctx, stream)
	for _, sfn := range sfns {
		next = pipeStreamFn(ctx, next, sfn)
	}

	return next
}

const bufferSize int = 100

// readDataFromSource reads data from source QUIC stream.
func readDataFromSource(ctx context.Context, stream quic.Stream) chan *frame.DataFrame {
	next := make(chan *frame.DataFrame, bufferSize)

	go func() {
		defer close(next)

	LOOP:
		for {
			select {
			case <-ctx.Done():
				return
			default:
				f, err := core.ParseFrame(stream)
				if err != nil {
					logger.Error("Parse the frame failed", "err", err)
					break LOOP
				}

				switch f.Type() {
				case frame.TagOfDataFrame:
					dataFrame := f.(*frame.DataFrame)
					logger.Debug("Receive data frame from source.", "TransactionID", dataFrame.TransactionID())
					next <- dataFrame
				default:
					logger.Debug("Only dispatch data frame to stream functions.", "type", f.Type())
				}
			}
		}
	}()

	return next
}

// pipeStreamFn sends the raw data to `stream-fn`, receives the new raw data and send it to next `stream-fn`.
func pipeStreamFn(ctx context.Context, upstream chan *frame.DataFrame, sfn GetStreamFunc) chan *frame.DataFrame {
	next := make(chan *frame.DataFrame, bufferSize)

	go func() {
		defer close(next)

		// send the stream to flow (zipper -> flow/sink)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case item, ok := <-upstream:
					if !ok {
						return
					}

					go dispatchToStreamFn(sfn, item, next)
				}
			}
		}()

		// receive the response from flow  (flow/sink -> zipper)
		receiveResponseFromStreamFn(ctx, sfn, next)
	}()

	return next
}

// dispatchToStreamFn dispatch the data from `upstream` to next `stream-fn` by Round Robin.
func dispatchToStreamFn(sfn GetStreamFunc, data *frame.DataFrame, next chan *frame.DataFrame) {
	var nextNum uint32

	name, funcs := sfn()
	len := len(funcs)
	// no available sessions in this stream-fn.
	if len == 0 {
		logger.Info("no available sessions in stream fn.", "name", name)
		return
	}

	// only one session in this stream-fn.
	if len == 1 {
		go sendDataToStreamFn(name, funcs[0].session, funcs[0].cancel, data, next)
		return
	}

	// get next session by Round Robin when has more sessions in this stream-fn.
	n := atomic.AddUint32(&nextNum, 1)
	i := (int(n) - 1) % len
	logger.Debug("[MergeStreamFunc] dispatch data to next stream-function", "name", name, "index", i)

	go sendDataToStreamFn(name, funcs[i].session, funcs[i].cancel, data, next)
}

// sendDataToStreamFn send the data to a specified `stream-fn` by QUIC Stream.
func sendDataToStreamFn(name string, session quic.Session, cancel CancelFunc, data *frame.DataFrame, next chan *frame.DataFrame) {
	if session == nil {
		logger.Error("[MergeStreamFunc] the session of the stream-function is nil", "stream-fn", name)
		// pass the data to next stream function if the current stream function is nil
		next <- data
		// cancel the current session when error.
		cancel()
		return
	}

	// tracing
	span := tracing.NewSpanFromData(string(data.GetCarriage()), name, "zipper-send-to-"+name)
	if span != nil {
		defer span.End()
	}

	// send data to downstream.
	stream, err := session.OpenUniStream()
	if err != nil {
		logger.Error("[MergeStreamFunc] session.OpenUniStream failed", "stream-fn", name, "err", err)
		// pass the data to next `stream function` if the current stream has error.
		next <- data
		// cancel the current session when error.
		cancel()
		return
	}

	_, err = stream.Write(data.Encode())
	stream.Close()
	if err != nil {
		logger.Error("[MergeStreamFunc] YoMo-Zipper sent data to `stream-fn` failed.", "stream-fn", name, "err", err)
		// cancel the current session when error.
		cancel()
		return
	}

	logger.Debug("[MergeStreamFunc] YoMo-Zipper sent data to `stream-fn`.", "stream-fn", name)
}

// receiveResponseFromStreamFn receives the response from `stream-fn`.
func receiveResponseFromStreamFn(ctx context.Context, sfn GetStreamFunc, next chan *frame.DataFrame) {
	name, _ := sfn()
	ch, _ := newStreamFuncSessionCache.LoadOrStore(name, make(chan quic.Session, 5))

	for {
		select {
		case <-ctx.Done():
			return
		case session, ok := <-ch.(chan quic.Session):
			if !ok {
				return
			}

			if session == nil {
				continue
			}

			go func() {
			LOOP_ACCP_STREAM:
				for {
					stream, err := session.AcceptUniStream(ctx)
					if err != nil {
						if err.Error() != quic.ErrConnectionClosed {
							logger.Error("[MergeStreamFunc] session.AcceptUniStream(ctx) failed", "stream-fn", name, "err", err)
						}
						break LOOP_ACCP_STREAM
					}

					go readDataFromStreamFn(ctx, name, stream, next)
				}
			}()
		}
	}
}

// readDataFromStreamFn reads the data from `stream-fn`.
func readDataFromStreamFn(ctx context.Context, name string, stream quic.ReceiveStream, next chan *frame.DataFrame) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			// 起始
			t1 := time.Now()
			logger.Printf("💚 waiting read next..")

			// 开始接收数据
			f, err := core.ParseFrame(stream)
			if err != nil {
				logger.Debug("[MergeStreamFunc] YoMo-Zipper received data from `stream-fn` failed.", "stream-fn", name, "err", err)
				return
			}

			logger.Debug("[MergeStreamFunc] YoMo-Zipper received data from `stream-fn`.", "stream-fn", name)

			// 完成接收
			if f.Type() != frame.TagOfDataFrame {
				logger.Debug("[MergeStreamFunc] YoMo-Zipper received frame from `stream-fn`, but the frame type is not a DataFrame.", "stream-fn", name, "type", f.Type().String())
				continue
			}

			data := f.(*frame.DataFrame)

			logger.Printf("💚 receive complete data(%d), duration=%d", len(data.GetCarriage()), time.Since(t1).Milliseconds())

			// if len(data) > 512 {
			// 	log.Printf("🔗 parsed out total %d bytes: \n\thead 64 bytes are: [%# x], \n\ttail 64 bytes are: [%# x]\n", len(data), data[0:64], data[len(data)-64:])
			// } else {
			// 	log.Printf("🔗 parsed out: [%# x]\n", data)
			// }

			// tracing
			span := tracing.NewSpanFromData(string(data.GetCarriage()), name, "zipper-receive-from-"+name)
			if span != nil {
				defer span.End()
			}

			// pass data to downstream.
			next <- data
			return
		}
	}
}
