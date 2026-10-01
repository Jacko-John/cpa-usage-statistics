package storage

import (
	"encoding/binary"
	"fmt"
)

func encodeTriggerTimes(times []int64) []byte {
	encoded := make([]byte, len(times)*8)
	for i, timestamp := range times {
		binary.LittleEndian.PutUint64(encoded[i*8:], uint64(timestamp))
	}
	return encoded
}

func decodeTriggerTimes(encoded []byte) ([]int64, error) {
	if len(encoded)%8 != 0 {
		return nil, fmt.Errorf("invalid recent trigger times")
	}
	times := make([]int64, len(encoded)/8)
	for i := range times {
		times[i] = int64(binary.LittleEndian.Uint64(encoded[i*8:]))
	}
	return times, nil
}
