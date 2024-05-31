package pq

import (
	"context"
	"fmt"
	"github.com/3n0ugh/dcpg/config"
	"github.com/3n0ugh/dcpg/pq/message"
	"github.com/3n0ugh/dcpg/pq/message/format"
	"github.com/go-playground/errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"log/slog"
	"time"
)

type ListenerContext struct {
	Message any
	Ack     func() error
}

type ListenerFunc func(ctx ListenerContext)

type Streamer interface {
	Open(ctx context.Context) error
	Close(ctx context.Context)
	GetSystemInfo() IdentifySystemResult
	GetMetric() *Metric
}

type stream struct {
	conn   Connection
	metric *Metric
	system IdentifySystemResult
	config config.Config

	relation     map[uint32]*format.Relation
	listenerCh   chan []byte
	listenerFunc ListenerFunc
}

func NewStream(conn Connection, cfg config.Config, system IdentifySystemResult, listenerFunc ListenerFunc) Streamer {
	return &stream{
		conn:         conn,
		metric:       &Metric{},
		system:       system,
		config:       cfg,
		relation:     make(map[uint32]*format.Relation),
		listenerCh:   make(chan []byte, cfg.ChannelBuffer),
		listenerFunc: listenerFunc,
	}
}

func (s *stream) Open(ctx context.Context) error {
	if err := s.replicationSetup(ctx); err != nil {
		return errors.Wrap(err, "replication setup")
	}

	s.sink(ctx)

	go s.listen(ctx)

	slog.Info("cdc stream started")

	return nil
}

func (s *stream) replicationSetup(ctx context.Context) error {
	replication := NewReplication(s.conn)

	if err := replication.Start(s.config.Publication.Name, s.config.Slot.Name); err != nil {
		return err
	}

	if err := replication.Test(ctx); err != nil {
		return err
	}

	slog.Info("replication started", "slot", s.config.Slot.Name)

	return nil
}

func (s *stream) sink(ctx context.Context) {
	slog.Info("postgres message sink started")
	lastXLogPos := LSN(10)

	go func() {
		for {
			msgCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second*10))
			rawMsg, err := s.conn.ReceiveMessage(msgCtx)
			cancel()
			if err != nil {
				if pgconn.Timeout(err) {
					err = SendStandbyStatusUpdate(ctx, s.conn, uint64(lastXLogPos))
					if err != nil {
						slog.Error("send stand by status update", "error", err)
						break
					}
					slog.Info("send stand by status update")
					continue
				}
				slog.Error("receive message error", "error", err)
				break
			}

			if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
				res, _ := errMsg.MarshalJSON()
				slog.Error("receive postgres wal error: " + string(res))
				continue
			}

			msg, ok := rawMsg.(*pgproto3.CopyData)
			if !ok {
				slog.Warn(fmt.Sprintf("received undexpected message: %T", rawMsg))
				continue
			}

			switch msg.Data[0] {
			case message.PrimaryKeepaliveMessageByteID:
				continue
			case message.XLogDataByteID:
				s.listenerCh <- msg.Data
			}
		}
	}()
}

func (s *stream) listen(ctx context.Context) {
	for data := range s.listenerCh {
		xld, err := ParseXLogData(data[1:])
		if err != nil {
			slog.Error("parse xLog data", "error", err)
			continue
		}

		s.metric.DcpLatency = time.Since(xld.ServerTime).Milliseconds()

		s.system.XLogPos = max(xld.WALStart, s.system.XLogPos)

		lCtx := ListenerContext{
			Ack: func() error {
				pos := s.system.XLogPos
				slog.Info("send stand by status update", "xLogPos", pos.String())
				return SendStandbyStatusUpdate(ctx, s.conn, uint64(pos))
			},
		}

		lCtx.Message, err = message.New(xld.WALData, s.relation)
		if err != nil || lCtx.Message == nil {
			slog.Error("wal data message parsing", "error", err)
			continue
		}

		switch lCtx.Message.(type) {
		case *format.Insert:
			s.metric.TotalInsert.Add(1)
		case *format.Delete:
			s.metric.TotalDelete.Add(1)
		case *format.Update:
			s.metric.TotalUpdate.Add(1)
		}

		start := time.Now()
		s.listenerFunc(lCtx)
		s.metric.ProcessLatency = time.Since(start).Milliseconds()
	}
}

func (s *stream) Close(ctx context.Context) {
	close(s.listenerCh)
	_ = s.conn.Close(ctx)
}

func (s *stream) GetSystemInfo() IdentifySystemResult {
	return s.system
}

func (s *stream) GetMetric() *Metric {
	return s.metric
}
