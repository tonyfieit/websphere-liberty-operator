// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"errors"
	"io"
	"sync"
)

// Decoder provides decoding of zstandard streams.
// The decoder has been designed to operate without allocations after a warmup.
// This means that you should store the decoder for best performance.
// To re-use a stream decoder, use the Reset(r io.Reader) error to switch to another stream.
// A decoder can safely be re-used even if the previous stream failed.
// To release the resources, you must call the Close() function on a decoder.
type Decoder struct {
	o decoderOptions

	// Unreferenced decoders, ready for use.
	decoders chan *blockDec

	// Unreferenced decoders, ready for use.
	frames chan *frameDec

	// Streams ready to be decoded.
	stream chan decodeStream

	// Current read position used for Reader functionality.
	current decoderState

	// Custom dictionaries
	dicts map[uint32]struct{}

	// streamWg is the waitgroup for all streams
	streamWg sync.WaitGroup
}

// decoderState is used for maintaining state when the decoder
// is used for streaming.
type decoderState struct {
	// current block being written to stream.
	decodeOutput

	// output in order to be written to stream.
	output chan decodeOutput

	// cancel remaining output.
	cancel chan struct{}

	flushed bool
}

var (
	// Check the interfaces we want to support.
	_ = io.WriterTo(&Decoder{})
	_ = io.Reader(&Decoder{})
)

// NewReader creates a new decoder.
// A nil Reader can be provided in which case Reset can be used to start a decode.
//
// A Decoder can be used in two modes:
//
// 1) As a stream, or
// 2) For stateless decoding using DecodeAll or DecodeBuffer.
//
// Only a single stream can be decoded concurrently, but the same decoder
// can run multiple concurrent stateless decodes. It is even possible to
// use stateless decodes while a stream is being decoded.
//
// The Reset function can be used to initiate a new stream, which is will considerably
// reduce the allocations normally caused by NewReader.
func NewReader(r io.Reader, opts ...DOption) (*Decoder, error) {
	var d Decoder
	d.o.setDefault()
	for _, o := range opts {
		err := o(&d.o)
		if err != nil {
			return nil, err
		}
	}
	d.current.output = make(chan decodeOutput, d.o.concurrent)
	d.current.flushed = true

	// Create decoders
	d.decoders = make(chan *blockDec, d.o.concurrent)
	d.frames = make(chan *frameDec, d.o.concurrent)
	for i := 0; i < d.o.concurrent; i++ {
		d.frames <- newFrameDec(d.o)
		d.decoders <- newBlockDec(d.o.lowMem)
	}

	if r == nil {
		return &d, nil
	}
	return &d, d.Reset(r)
}

// Read bytes from the decompressed stream into p.
// Returns the number of bytes written and any error that occurred.
// When the stream is done, io.EOF will be returned.
func (d *Decoder) Read(p []byte) (int, error) {
	if d.stream == nil {
		return 0, errors.New("no input has been initialized")
	}
	var n int
	for {
		if len(d.current.b) > 0 {
			filled := copy(p, d.current.b)
			p = p[filled:]
			d.current.b = d.current.b[filled:]
			n += filled
		}
		if len(p) == 0 {
			break
		}
		if len(d.current.b) == 0 {
			// We have an error and no more data
			if d.current.err != nil {
				break
			}
			d.nextBlock()
		}
	}
	if len(d.current.b) > 0 {
		// Only return error at end of block
		return n, nil
	}
	if d.current.err != nil {
		d.drainOutput()
	}
	if debug {
		println("returning", n, d.current.err, len(d.decoders))
	}
	return n, d.current.err
}

// Reset will reset the decoder the supplied stream after the current has finished processing.
// Note that this functionality cannot be used after Close has been called.
func (d *Decoder) Reset(r io.Reader) error {
	if d.current.err == ErrDecoderClosed {
		return d.current.err
	}
	if r == nil {
		return errors.New("nil Reader sent as input")
	}

	// TODO: If r is a *bytes.Buffer, we could automatically switch to sync operation.

	if d.stream == nil {
		d.stream = make(chan decodeStream, 1)
		go d.startStreamDecoder(d.stream)
	}

	d.drainOutput()

	// Remove current block.
	d.current.decodeOutput = decodeOutput{}
	d.current.err = nil
	d.current.cancel = make(chan struct{})
	d.current.flushed = false
	d.current.d = nil

	d.stream <- decodeStream{
		r:      r,
		output: d.current.output,
		cancel: d.current.cancel,
	}
	return nil
}

// drainOutput will drain the output until errEndOfStream is sent.
func (d *Decoder) drainOutput() {
	if d.current.cancel != nil {
		println("cancelling current")
		close(d.current.cancel)
		d.current.cancel = nil
	}
	if d.current.d != nil {
		println("re-adding current decoder", d.current.d, len(d.decoders))
		d.decoders <- d.current.d
		d.current.d = nil
		d.current.b = nil
	}
	if d.current.output == nil || d.current.flushed {
		println("current already flushed")
		return
	}
	for {
		select {
		case v := <-d.current.output:
			if v.d != nil {
				println("got decoder", v.d)
				d.decoders <- v.d
			}
			if v.err == errEndOfStream {
				println("current flushed")
				d.current.flushed = true
				return
			}
		}
	}
}

// WriteTo writes data to w until there's no more data to write or when an error occurs.
// The return value n is the number of bytes written.
// Any error encountered during the write is also returned.
func (d *Decoder) WriteTo(w io.Writer) (int64, error) {
	if d.stream == nil {
		return 0, errors.New("no input has been initialized")
	}
	var n int64
	for d.current.err == nil {
		if len(d.current.b) > 0 {
			n2, err2 := w.Write(d.current.b)
			n += int64(n2)
			if err2 != nil && d.current.err == nil {
				d.current.err = err2
				break
			}
		}
		d.nextBlock()
	}
	err := d.current.err
	if err != nil {
		d.drainOutput()
	}
	if err == io.EOF {
		err = nil
	}
	return n, err
}

// DecodeAll allows stateless decoding of a blob of bytes.
// Output will be appended to dst, so if the destination size is known
// you can pre-allocate the destination slice to avoid allocations.
// DecodeAll can be used concurrently.
// The Decoder concurrency limits will be respected.
func (d *Decoder) DecodeAll(input, dst []byte) ([]byte, error) {
	if d.current.err == ErrDecoderClosed {
		return dst, ErrDecoderClosed
	}
	//println(len(d.frames), len(d.decoders), d.current)
	block, frame := <-d.decoders, <-d.frames
	defer func() {
		d.decoders <- block
		frame.rawInput = nil
		d.frames <- frame
	}()
	if cap(dst) == 0 {
		// Allocate 1MB by default.
		dst = make([]byte, 0, 1<<20)
	}
	br := byteBuf(input)
	for {
		err := frame.reset(&br)
		if err == io.EOF {
			return dst, nil
		}
		if err != nil {
			return dst, err
		}
		if frame.FrameContentSize > d.o.maxDecodedSize-uint64(len(dst)) {
			return dst, ErrDecoderSizeExceeded
		}
		if frame.FrameContentSize > 0 && frame.FrameContentSize < 1<<30 {
			// Never preallocate moe than 1 GB up front.
			if uint64(cap(dst)) < frame.FrameContentSize {
				dst2 := make([]byte, len(dst), len(dst)+int(frame.FrameContentSize))
				copy(dst2, dst)
				dst = dst2
			}
		}
		dst, err = frame.runDecoder(dst, block)
		if err != nil {
			return dst, err
		}
		if len(br) == 0 {
			break
		}
	}
	return dst, nil
}

// nextBlock returns the next block.
// If an error occurs d.err will be set.
func (d *Decoder) nextBlock() {
	if d.current.d != nil {
		d.decoders <- d.current.d
		d.current.d = nil
	}
	if d.current.err != nil {
		// Keep error state.
		return
	}
	d.current.decodeOutput = <-d.current.output
	if debug {
		println("got", len(d.current.b), "bytes, error:", d.current.err)
	}
}

// Close will release all resources.
// It is NOT possible to reuse the decoder after this.
func (d *Decoder) Close() {
	if d.current.err == ErrDecoderClosed {
		return
	}
	d.drainOutput()
	if d.stream != nil {
		close(d.stream)
		d.streamWg.Wait()
		d.stream = nil
	}
	if d.decoders != nil {
		close(d.decoders)
		for dec := range d.decoders {
			dec.Close()
		}
		d.decoders = nil
	}
	if d.current.d != nil {
		d.current.d.Close()
		d.current.d = nil
	}
	d.current.err = ErrDecoderClosed
}

type decodeOutput struct {
	d   *blockDec
	b   []byte
	err error
}

type decodeStream struct {
	r io.Reader

	// Blocks ready to be written to output.
	output chan decodeOutput

	// cancel reading from the input
	cancel chan struct{}
}

// errEndOfStream indicates that everything from the stream was read.
var errEndOfStream = errors.New("end-of-stream")

// Create Decoder:
// Spawn n block decoders. These accept tasks to decode a block.
// Create goroutine that handles stream processing, this will send history to decoders as they are available.
// Decoders update the history as they decode.
// When a block is returned:
// 		a) history is sent to the next decoder,
// 		b) content written to CRC.
// 		c) return data to WRITER.
// 		d) wait for next block to return data.
// Once WRITTEN, the decoders reused by the writer frame decoder for re-use.
func (d *Decoder) startStreamDecoder(inStream chan decodeStream) {
	d.streamWg.Add(1)
	defer d.streamWg.Done()
	frame := newFrameDec(d.o)
	for stream := range inStream {
		br := readerWrapper{r: stream.r}
	decodeStream:
		for {
			err := frame.reset(&br)
			println("Frame decoder returned", err)
			if err != nil {
				stream.output <- decodeOutput{
					err: err,
				}
				break
			}
			println("starting frame decoder")

			// This goroutine will forward history between frames.
			frame.frameDone.Add(1)
			frame.initAsync()

			go frame.startDecoder(stream.output)
		decodeFrame:
			// Go through all blocks of the frame.
			for {
				dec := <-d.decoders
				select {
				case <-stream.cancel:
					if !frame.sendErr(dec, io.EOF) {
						// To not let the decoder dangle, send it back.
						stream.output <- decodeOutput{d: dec}
					}
					break decodeStream
				default:
				}
				err := frame.next(dec)
				switch err {
				case io.EOF:
					// End of current frame, no error
					println("EOF on next block")
					break decodeFrame
				case nil:
					continue
				default:
					println("block decoder returned", err)
					break decodeStream
				}
			}
			// All blocks have started decoding, check if there are more frames.
			println("waiting for done")
			frame.frameDone.Wait()
			println("done waiting...")
		}
		frame.frameDone.Wait()
		println("Sending EOS")
		stream.output <- decodeOutput{err: errEndOfStream}
	}
}
