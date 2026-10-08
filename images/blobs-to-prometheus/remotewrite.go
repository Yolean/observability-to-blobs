package main

// Prometheus remote write 1.0: a snappy-compressed prompb.WriteRequest,
// encoded by hand (protoBuf) to keep the dependency set small.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/golang/snappy"
)

// errRejected is a 4xx answer other than 429: the receiver read the
// request and refuses (part of) its samples, so sending it again cannot
// help.
var errRejected = errors.New("rejected")

type remoteWriter struct {
	url    string
	client *http.Client
}

// encode builds a WriteRequest of the given series of b.
func encode(b *batch, keys []string) []byte {
	var wr protoBuf
	for _, key := range keys {
		sr := b.series[key]
		var ts protoBuf
		for _, l := range sr.labels {
			var lbl protoBuf
			lbl.tagBytes(1, []byte(l.name))
			lbl.tagBytes(2, []byte(l.value))
			ts.tagMessage(1, lbl.Bytes())
		}
		for _, s := range sr.samples {
			var smp protoBuf
			smp.tagDouble(1, s.value)
			smp.tagVarint(2, uint64(s.ts))
			ts.tagMessage(2, smp.Bytes())
		}
		wr.tagMessage(1, ts.Bytes())
	}
	return wr.Bytes()
}

// push sends a batch. When the receiver rejects the request as a whole,
// which Prometheus does for one out-of-order or too-old sample, every
// series is sent on its own so that only the series it refuses are lost;
// dropped is their count. A network error, a 5xx or a 429 is returned
// for the caller to retry the whole batch later.
func (w *remoteWriter) push(ctx context.Context, b *batch) (dropped int, err error) {
	if b.samples == 0 {
		return 0, nil
	}
	err = w.send(ctx, encode(b, b.order))
	if !errors.Is(err, errRejected) {
		return 0, err
	}
	first := err
	for _, key := range b.order {
		if err := w.send(ctx, encode(b, []string{key})); err != nil {
			if !errors.Is(err, errRejected) {
				return dropped, err
			}
			dropped++
		}
	}
	if dropped > 0 {
		return dropped, fmt.Errorf("%d of %d series refused (%v)", dropped, len(b.order), first)
	}
	return 0, nil
}

func (w *remoteWriter) send(ctx context.Context, wr []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(snappy.Encode(nil, wr)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err = fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusTooManyRequests {
		return fmt.Errorf("%w: %v", errRejected, err)
	}
	return err
}

// protoBuf is a minimal protobuf wire-format writer, enough for
// prometheus.WriteRequest: message fields (length-delimited), bytes,
// varints and doubles. Field numbers follow prompb/remote.proto and
// prompb/types.proto.
type protoBuf struct {
	bytes.Buffer
}

func (p *protoBuf) varint(v uint64) {
	for v >= 0x80 {
		p.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	p.WriteByte(byte(v))
}

func (p *protoBuf) tagVarint(field int, v uint64) {
	p.varint(uint64(field)<<3 | 0)
	p.varint(v)
}

func (p *protoBuf) tagDouble(field int, v float64) {
	p.varint(uint64(field)<<3 | 1)
	var raw [8]byte
	bits := math.Float64bits(v)
	for i := 0; i < 8; i++ {
		raw[i] = byte(bits >> (8 * i))
	}
	p.Write(raw[:])
}

func (p *protoBuf) tagBytes(field int, b []byte) {
	p.varint(uint64(field)<<3 | 2)
	p.varint(uint64(len(b)))
	p.Write(b)
}

func (p *protoBuf) tagMessage(field int, b []byte) {
	p.tagBytes(field, b)
}
