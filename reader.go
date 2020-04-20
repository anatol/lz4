package lz4

import (
	"io"
)

var readerStates = []aState{
	noState:     newState,
	newState:    headerState,
	headerState: readState,
	readState:   closedState,
	closedState: newState,
	errorState:  newState,
}

// NewReader returns a new LZ4 frame decoder.
func NewReader(r io.Reader) io.Reader {
	zr := &_Reader{src: r}
	zr.state.init(readerStates)
	return zr
}

type _Reader struct {
	state _State
	buf   [9]byte   // frame descriptor needs at most 8+1=9 bytes
	src   io.Reader // source reader
	frame Frame     // frame being read
	data  []byte    // pending data
	idx   int       // size of pending data
}

// Size returns the size of the underlying uncompressed data, if set in the stream.
func (r *_Reader) Size() int {
	switch r.state.state {
	case readState, closedState:
		if r.frame.Descriptor.Flags.Size() {
			return int(r.frame.Descriptor.ContentSize)
		}
	}
	return 0
}

func (r *_Reader) Read(buf []byte) (n int, err error) {
	defer r.state.check(&err)
	switch r.state.state {
	case closedState, errorState:
		return 0, r.state.err
	case newState:
		// First initialization.
		r.state.next(nil)
		if err = r.frame.initR(r); r.state.next(err) {
			return
		}
		r.state.next(nil)
		r.data = r.frame.Descriptor.Flags.BlockSizeIndex().get()
	default:
		return 0, r.state.fail()
	}
	if len(buf) == 0 {
		return
	}

	if r.idx > 0 {
		// Some left over data, use it.
		bn := copy(buf, r.data[r.idx:])
		n += bn
		r.idx += bn
		if r.idx == len(r.data) {
			// All data read, get ready for the next Read.
			r.idx = 0
		}
		return
	}
	// No uncompressed data yet.
	var bn int
	for len(buf) >= len(r.data) {
		// Input buffer large enough and no pending data: uncompress directly into it.
		switch bn, err = r.frame.Blocks.Block.uncompress(r, buf); err {
		case nil:
			n += bn
			buf = buf[bn:]
		case io.EOF:
			goto close
		default:
			return
		}
	}
	if n > 0 {
		// Some data was read, done for now.
		return
	}
	// Read the next block.
	switch bn, err = r.frame.Blocks.Block.uncompress(r, r.data); err {
	case nil:
		n += bn
	case io.EOF:
		goto close
	}
	return
close:
	n += bn
	err = r.frame.closeR(r)
	r.frame.Descriptor.Flags.BlockSizeIndex().put(r.data)
	r.reset(nil)
	return
}

func (r *_Reader) reset(reader io.Reader) {
	r.src = reader
	r.data = nil
	r.idx = 0
}

// Reset clears the state of the Reader r such that it is equivalent to its
// initial state from NewReader, but instead writing to writer.
// No access to reader is performed.
//
// w.Close must be called before Reset.
func (r *_Reader) Reset(reader io.Reader) {
	r.reset(reader)
	r.state.state = noState
	r.state.next(nil)
}

func (r *_Reader) Seek(offset int64, whence int) (int64, error) {
	panic("TODO")
}
