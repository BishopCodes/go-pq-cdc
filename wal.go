package dcpg

import (
	"encoding/binary"
	"fmt"
	"time"
)

const microSecFromUnixEpochToY2K = 946684800 * 1000000

type XLogData struct {
	WALStart     LSN
	ServerWALEnd LSN
	ServerTime   time.Time
	WALData      []byte
}

func ParseXLogData(buf []byte) (XLogData, error) {
	var xld XLogData
	if len(buf) < 24 {
		return xld, fmt.Errorf("XLogData must be at least 24 bytes, got %d", len(buf))
	}

	xld.WALStart = LSN(binary.BigEndian.Uint64(buf))
	xld.ServerWALEnd = LSN(binary.BigEndian.Uint64(buf[8:]))
	xld.ServerTime = pgTimeToTime(int64(binary.BigEndian.Uint64(buf[16:])))
	xld.WALData = buf[24:]

	return xld, nil
}

func pgTimeToTime(microSecSinceY2K int64) time.Time {
	return time.Unix(0, microSecFromUnixEpochToY2K+microSecSinceY2K*1000)
}
