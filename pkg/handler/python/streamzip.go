package python

import (
	"bufio"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// errStreamMETADATAUnreachable means the wheel zip used a feature the
// streaming walker does not decode (encryption, an unsupported
// compression method, ZIP64 sizes in a local file header, STORED with
// a streaming data descriptor, etc.). The python upload handler
// degrades gracefully on this error: the wheel still publishes; the
// PEP 658 .metadata sibling is just not produced.
var errStreamMETADATAUnreachable = errors.New("wheel METADATA cannot be reached via streaming walker")

// errMalformedZip is returned when the bytes read from the stream do
// not look like a valid zip local file header sequence.
var errMalformedZip = errors.New("malformed zip stream")

const (
	zipMethodStored   uint16 = 0
	zipMethodDeflated uint16 = 8

	zipFlagEncrypted      uint16 = 1 << 0
	zipFlagDataDescriptor uint16 = 1 << 3

	sigLocalFileHeader  uint32 = 0x04034b50
	sigCentralDirectory uint32 = 0x02014b50
	sigEOCD             uint32 = 0x06054b50
	sigDataDescriptor   uint32 = 0x08074b50

	zip64SizeSentinel uint32 = 0xFFFFFFFF

	localFileHeaderFixed = 30
)

// extractWheelMetadataStream walks zip local file headers in stream
// order and returns the bytes of the wheel's `<distinfo>/METADATA`
// entry. It does not seek and never buffers the full archive: entries
// other than METADATA are read and discarded as they pass; METADATA
// itself is decompressed into a maxMetadataSize-capped buffer.
//
// When METADATA is found the walker returns immediately — it does not
// continue past that entry. Callers must drain the upstream reader
// independently (e.g. by reading the io.Pipe behind a TeeReader) so
// the upload writer side can complete.
//
// errMetadataNotFound is returned when the archive ends (or hits the
// central directory) without a METADATA entry.
// errWheelTooManyEntries is returned when the local file header count
// exceeds maxWheelEntries.
// errStreamMETADATAUnreachable is returned when the archive uses a
// zip feature the walker does not decode; the caller should treat it
// as best-effort and continue without the .metadata companion.
func extractWheelMetadataStream(r io.Reader) ([]byte, error) {
	br := bufio.NewReaderSize(r, 8*1024)
	entries := 0
	for {
		sigBytes, err := br.Peek(4)
		if errors.Is(err, io.EOF) {
			return nil, errMetadataNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("peek signature: %w", err)
		}
		sig := binary.LittleEndian.Uint32(sigBytes)
		if sig == sigCentralDirectory || sig == sigEOCD {
			return nil, errMetadataNotFound
		}
		if sig != sigLocalFileHeader {
			return nil, errMalformedZip
		}

		entries++
		if entries > maxWheelEntries {
			return nil, errWheelTooManyEntries
		}

		var hdr [localFileHeaderFixed]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return nil, fmt.Errorf("local file header: %w", err)
		}

		flags := binary.LittleEndian.Uint16(hdr[6:])
		method := binary.LittleEndian.Uint16(hdr[8:])
		csize := binary.LittleEndian.Uint32(hdr[18:])
		usize := binary.LittleEndian.Uint32(hdr[22:])
		fnameLen := binary.LittleEndian.Uint16(hdr[26:])
		extraLen := binary.LittleEndian.Uint16(hdr[28:])

		if flags&zipFlagEncrypted != 0 {
			return nil, errStreamMETADATAUnreachable
		}
		streaming := flags&zipFlagDataDescriptor != 0

		fname := make([]byte, fnameLen)
		if _, err := io.ReadFull(br, fname); err != nil {
			return nil, fmt.Errorf("local file header name: %w", err)
		}
		if extraLen > 0 {
			if _, err := io.CopyN(io.Discard, br, int64(extraLen)); err != nil {
				return nil, fmt.Errorf("local file header extra: %w", err)
			}
		}

		if wheelMetadataPath.Match(fname) {
			if !streaming && usize > maxMetadataSize {
				return nil, fmt.Errorf("METADATA declared size %d exceeds cap %d", usize, maxMetadataSize)
			}
			return decodeStreamingMETADATAEntry(br, method, streaming, csize)
		}

		if err := skipStreamingEntryBody(br, method, streaming, csize); err != nil {
			return nil, err
		}
		if streaming {
			if err := skipDataDescriptor(br); err != nil {
				return nil, err
			}
		}
	}
}

// decodeStreamingMETADATAEntry reads the body of the METADATA entry
// and returns its uncompressed bytes. csize is meaningful only when
// streaming is false; for streaming entries the codec drives end-of-
// stream detection.
func decodeStreamingMETADATAEntry(br *bufio.Reader, method uint16, streaming bool, csize uint32) ([]byte, error) {
	if !streaming && csize == zip64SizeSentinel {
		return nil, errStreamMETADATAUnreachable
	}

	var rdr io.ReadCloser
	switch method {
	case zipMethodStored:
		if streaming {
			// STORED with a data descriptor has no codec-driven end
			// marker; without csize there's no way to know where the
			// entry ends. Bail.
			return nil, errStreamMETADATAUnreachable
		}
		rdr = io.NopCloser(io.LimitReader(br, int64(csize)))
	case zipMethodDeflated:
		// flate.NewReader reads byte-at-a-time off an io.ByteReader
		// (which *bufio.Reader is) and stops at the deflate end-of-
		// stream marker, so the underlying reader stays positioned
		// for the next walker step. No explicit csize bound needed.
		rdr = flate.NewReader(br)
	default:
		return nil, errStreamMETADATAUnreachable
	}
	defer rdr.Close()

	data, err := io.ReadAll(io.LimitReader(rdr, maxMetadataSize+1))
	if err != nil {
		return nil, fmt.Errorf("decode METADATA: %w", err)
	}
	if int64(len(data)) > maxMetadataSize {
		return nil, fmt.Errorf("METADATA exceeds %d bytes", maxMetadataSize)
	}
	return data, nil
}

// skipStreamingEntryBody discards the body of a non-METADATA entry.
// For non-streaming entries with a known csize this is a CopyN; for
// streaming-mode DEFLATE entries the deflate end-of-stream marker is
// the only delimiter so flate.NewReader is used.
func skipStreamingEntryBody(br *bufio.Reader, method uint16, streaming bool, csize uint32) error {
	if streaming {
		switch method {
		case zipMethodStored:
			return errStreamMETADATAUnreachable
		case zipMethodDeflated:
			fr := flate.NewReader(br)
			_, err := io.Copy(io.Discard, fr)
			closeErr := fr.Close()
			if err != nil {
				return fmt.Errorf("skip deflate body: %w", err)
			}
			if closeErr != nil {
				return fmt.Errorf("close deflate skip: %w", closeErr)
			}
			return nil
		default:
			return errStreamMETADATAUnreachable
		}
	}
	if csize == zip64SizeSentinel {
		return errStreamMETADATAUnreachable
	}
	if _, err := io.CopyN(io.Discard, br, int64(csize)); err != nil {
		return fmt.Errorf("skip body: %w", err)
	}
	return nil
}

// skipDataDescriptor consumes the post-body data descriptor that
// follows a streaming-mode entry. The descriptor is either 12 bytes
// (crc32 + csize + usize) or 16 bytes when prefixed with the optional
// 0x08074b50 signature.
func skipDataDescriptor(br *bufio.Reader) error {
	sigBytes, err := br.Peek(4)
	if err != nil {
		return fmt.Errorf("data descriptor: %w", err)
	}
	n := 12
	if binary.LittleEndian.Uint32(sigBytes) == sigDataDescriptor {
		n = 16
	}
	if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
		return fmt.Errorf("data descriptor body: %w", err)
	}
	return nil
}
