package lz4

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/pierrec/lz4/internal/xxh32"
)

//go:generate go run gen.go

type Frame struct {
	Magic      uint32
	Descriptor FrameDescriptor
	Blocks     Blocks
	Checksum   uint32
	checksum   xxh32.XXHZero
}

func (f *Frame) initW(w *_Writer) {
	f.Magic = frameMagic
	f.Descriptor.initW(w)
	f.Blocks.initW(w)
	f.checksum.Reset()
}

func (f *Frame) closeW(w *_Writer) error {
	if err := f.Blocks.closeW(w); err != nil {
		return err
	}
	buf := w.buf[:0]
	if f.Descriptor.Flags.ContentChecksum() {
		buf = f.checksum.Sum(buf)
	}
	// End mark (data block size of uint32(0)).
	buf = append(buf, 0, 0, 0, 0)
	_, err := w.src.Write(buf)
	return err
}

func (f *Frame) initR(r *_Reader) error {
	if f.Magic > 0 {
		// Header already read.
		return nil
	}
newFrame:
	if err := readUint32(r.src, r.buf[:], &f.Magic); err != nil {
		return err
	}
	switch m := f.Magic; {
	case m == frameMagic:
	// All 16 values of frameSkipMagic are valid.
	case m>>8 == frameSkipMagic>>8:
		var skip uint32
		if err := binary.Read(r.src, binary.LittleEndian, &skip); err != nil {
			return err
		}
		if _, err := io.CopyN(ioutil.Discard, r.src, int64(skip)); err != nil {
			return err
		}
		goto newFrame
	default:
		return ErrInvalid
	}
	if err := f.Descriptor.initR(r); err != nil {
		return err
	}
	f.Blocks.initR(r)
	f.checksum.Reset()
	return nil
}

func (f *Frame) closeR(r *_Reader) error {
	f.Magic = 0
	if !f.Descriptor.Flags.ContentChecksum() {
		return nil
	}
	if err := readUint32(r.src, r.buf[:], &f.Checksum); err != nil {
		return err
	}
	if c := f.checksum.Sum32(); c != f.Checksum {
		return fmt.Errorf("%w: got %x; expected %x", ErrInvalidFrameChecksum, c, f.Checksum)
	}
	return nil
}

type FrameDescriptor struct {
	Flags       DescriptorFlags
	ContentSize uint64
	Checksum    uint8
}

func (fd *FrameDescriptor) initW(_ *_Writer) {
	fd.Flags.VersionSet(1)
	fd.Flags.BlockIndependenceSet(false)
}

func (fd *FrameDescriptor) write(w *_Writer) error {
	if fd.Checksum > 0 {
		// Header already written.
		return nil
	}

	buf := w.buf[:]
	binary.LittleEndian.PutUint16(buf, uint16(fd.Flags))

	var checksum uint32
	if fd.Flags.Size() {
		checksum = xxh32.ChecksumZero10(uint16(fd.Flags), fd.ContentSize)
		binary.LittleEndian.PutUint64(buf[2:], fd.ContentSize)
		buf = buf[:10]
	} else {
		checksum = xxh32.Uint32Zero(uint32(fd.Flags))
		buf = buf[:2]
	}
	fd.Checksum = byte(checksum >> 8)
	buf = append(buf, fd.Checksum)

	_, err := w.src.Write(buf)
	return err
}

func (fd *FrameDescriptor) initR(r *_Reader) error {
	buf := r.buf[:2]
	if _, err := io.ReadFull(r.src, buf); err != nil {
		return err
	}
	descr := binary.LittleEndian.Uint64(buf)
	fd.Flags = DescriptorFlags(descr)

	var checksum uint32
	if fd.Flags.Size() {
		buf = buf[:9]
		if _, err := io.ReadFull(r.src, buf); err != nil {
			return err
		}
		fd.ContentSize = binary.LittleEndian.Uint64(buf)
		checksum = xxh32.ChecksumZero10(uint16(fd.Flags), fd.ContentSize)
	} else {
		buf = buf[:1]
		var err error
		if br, ok := r.src.(io.ByteReader); ok {
			buf[0], err = br.ReadByte()
		} else {
			_, err = io.ReadFull(r.src, buf)
		}
		if err != nil {
			return err
		}
		checksum = xxh32.Uint32Zero(uint32(fd.Flags))
	}
	fd.Checksum = buf[len(buf)-1]
	if c := byte(checksum >> 8); fd.Checksum != c {
		return fmt.Errorf("lz4: %w: got %x; expected %x", ErrInvalidHeaderChecksum, c, fd.Checksum)
	}

	return nil
}

type Blocks struct {
	Block  *FrameDataBlock
	Blocks chan chan *FrameDataBlock
	err    error
}

func (b *Blocks) initW(w *_Writer) {
	size := w.frame.Descriptor.Flags.BlockSizeIndex()
	if w.isNotConcurrent() {
		b.Blocks = nil
		b.Block = newFrameDataBlock(size)
		return
	}
	if cap(b.Blocks) != w.num {
		b.Blocks = make(chan chan *FrameDataBlock, w.num)
	}
	// goroutine managing concurrent block compression goroutines.
	go func() {
		// Process next block compression item.
		for c := range b.Blocks {
			// Read the next compressed block result.
			// Waiting here ensures that the blocks are output in the order they were sent.
			// The incoming channel is always closed as it indicates to the caller that
			// the block has been processed.
			block := <-c
			if block == nil {
				// Notify the block compression routine that we are done with its result.
				// This is used when a sentinel block is sent to terminate the compression.
				close(c)
				return
			}
			// Do not attempt to write the block upon any previous failure.
			if b.err == nil {
				// Write the block.
				if err := block.write(w); err != nil && b.err == nil {
					// Keep the first error.
					b.err = err
					// All pending compression goroutines need to shut down, so we need to keep going.
				}
			}
			close(c)
		}
	}()
}

func (b *Blocks) closeW(w *_Writer) error {
	if w.isNotConcurrent() {
		b.Block.closeW(w)
		b.Block = nil
		return nil
	}
	c := make(chan *FrameDataBlock)
	b.Blocks <- c
	c <- nil
	<-c
	err := b.err
	b.err = nil
	return err
}

func (b *Blocks) initR(r *_Reader) {
	size := r.frame.Descriptor.Flags.BlockSizeIndex()
	b.Block = newFrameDataBlock(size)
}

func newFrameDataBlock(size BlockSizeIndex) *FrameDataBlock {
	return &FrameDataBlock{Data: size.get()}
}

type FrameDataBlock struct {
	Size     DataBlockSize
	Data     []byte
	Checksum uint32
}

func (b *FrameDataBlock) closeW(w *_Writer) {
	size := w.frame.Descriptor.Flags.BlockSizeIndex()
	size.put(b.Data)
}

// Block compression errors are ignored since the buffer is sized appropriately.
func (b *FrameDataBlock) compress(w *_Writer, src []byte, ht []int) *FrameDataBlock {
	dst := b.Data
	var n int
	switch w.level {
	case Fast:
		n, _ = CompressBlock(src, dst, ht)
	default:
		n, _ = CompressBlockHC(src, dst, w.level, ht)
	}
	if n == 0 {
		b.Size.compressedSet(false)
		dst = src
	} else {
		b.Size.compressedSet(true)
		dst = dst[:n]
	}
	b.Data = dst
	b.Size.sizeSet(len(dst))

	if w.frame.Descriptor.Flags.BlockChecksum() {
		b.Checksum = xxh32.ChecksumZero(src)
	}
	if w.frame.Descriptor.Flags.ContentChecksum() {
		_, _ = w.frame.checksum.Write(src)
	}
	return b
}

func (b *FrameDataBlock) write(w *_Writer) error {
	buf := w.buf[:]
	out := w.src

	binary.LittleEndian.PutUint32(buf, uint32(b.Size))
	if _, err := out.Write(buf[:4]); err != nil {
		return err
	}

	if _, err := out.Write(b.Data); err != nil {
		return err
	}

	if b.Checksum == 0 {
		return nil
	}
	binary.LittleEndian.PutUint32(buf, b.Checksum)
	_, err := out.Write(buf[:4])
	return err
}

func (b *FrameDataBlock) uncompress(r *_Reader, dst []byte) (int, error) {
	var x uint32
	if err := readUint32(r.src, r.buf[:], &x); err != nil {
		return 0, err
	}
	b.Size = DataBlockSize(x)
	if b.Size == 0 {
		return 0, io.EOF
	}

	isCompressed := b.Size.compressed()
	var data []byte
	if isCompressed {
		data = b.Data
	} else {
		data = dst
	}
	if _, err := io.ReadFull(r.src, data[:b.Size.size()]); err != nil {
		return 0, err
	}
	if isCompressed {
		n, err := UncompressBlock(data, dst)
		if err != nil {
			return 0, err
		}
		data = dst[:n]
	}

	if r.frame.Descriptor.Flags.BlockChecksum() {
		if err := readUint32(r.src, r.buf[:], &b.Checksum); err != nil {
			return 0, err
		}
		if c := xxh32.ChecksumZero(data); c != b.Checksum {
			return 0, fmt.Errorf("lz4: %w: got %x; expected %x", ErrInvalidBlockChecksum, c, b.Checksum)
		}
	}
	if r.frame.Descriptor.Flags.ContentChecksum() {
		_, _ = r.frame.checksum.Write(data)
	}
	return len(data), nil
}

func readUint32(r io.Reader, buf []byte, x *uint32) error {
	if _, err := io.ReadFull(r, buf[:4]); err != nil {
		return err
	}
	*x = binary.LittleEndian.Uint32(buf)
	return nil
}
