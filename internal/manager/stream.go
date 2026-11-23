package manager

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jijiechen/turing-monitor/internal/capture"
	"github.com/jijiechen/turing-monitor/internal/usb"
)

// streamToPanel runs one streaming session: it primes the panel and feeds it
// frames until the panel goes away or the context is cancelled.
//
// The panel resets when it is unplugged, so the prelude has to be sent again on
// every reconnect; that is why it lives here rather than alongside the capture
// setup.
//
// The loop is pipelined: the previous frame is sent while the next is being
// encoded, which hides the USB round trip behind capture latency. See
// framerate-issues.md for the measurement history behind this shape, and note
// the frame copy -- the capture session reuses one buffer, so the bytes must
// be owned before the send goroutine touches them.
func streamToPanel(ctx context.Context, dev *usb.Device, session *capture.Session, fps int, log io.Writer) error {
	if err := dev.BeginPlayback(fps); err != nil {
		return err
	}
	fmt.Fprintf(log, "streaming to %s\n", dev.Model())

	frames, bytes := 0, 0
	start := time.Now()
	lastReport := start
	var waitCap, waitSend time.Duration
	winFrames := 0

	// Prime the pipeline with one encoded frame, copied out of the capture
	// buffer before the send goroutine takes it.
	frame, err := session.Next()
	if err != nil {
		return err
	}
	out := make([]byte, len(frame))
	copy(out, frame)

	sendErr := make(chan error, 1)
	go func() {
		sendErr <- dev.SendVideoFrame(out)
	}()

	for {
		if ctx.Err() != nil {
			// End the stream properly so the panel is not left mid-session.
			if err := <-sendErr; err != nil {
				return err
			}
			_ = dev.StopStream()
			fmt.Fprintf(log, "stopped after %d frames\n", frames)
			return nil
		}

		t0 := time.Now()
		next, err := session.Next()
		waitCap += time.Since(t0)
		if err != nil {
			<-sendErr
			return err
		}

		// The previous send must finish before this frame's bytes are put in
		// the shared buffer; the device's endpoints take one transfer at a
		// time and the copy target is the send goroutine's previous input.
		t1 := time.Now()
		if err := <-sendErr; err != nil {
			return err
		}
		waitSend += time.Since(t1)
		if cap(out) < len(next) {
			out = make([]byte, len(next))
		}
		out = out[:len(next)]
		copy(out, next)
		send := out
		go func() {
			sendErr <- dev.SendVideoFrame(send)
		}()
		frames++
		winFrames++
		bytes += len(send)

		// Once a second, report enough to tell "the encoder is starving for
		// bits" apart from "the panel cannot keep up".
		if d := time.Since(lastReport); d >= time.Second {
			fmt.Fprintf(log, "frames=%6d  fps=%5.1f  rate=%6.0f KB/s  queue=%d  capwait=%.1fms/f sendwait=%.1fms/f\n",
				frames, float64(winFrames)/d.Seconds(),
				float64(bytes)/1024/time.Since(start).Seconds(), dev.QueueDepth(),
				waitCap.Seconds()*1000/float64(winFrames), waitSend.Seconds()*1000/float64(winFrames))
			lastReport = time.Now()
			winFrames = 0
			waitCap, waitSend = 0, 0
		}
	}
}
