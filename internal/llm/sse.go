package llm

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
)

var errStreamInterrupted = errors.New("provider stream interrupted")

const maxSSEEventBytes = 1 << 20

type sseFrame struct {
	Event string
	Data  string
}

func readSSE(ctx context.Context, r io.Reader, emit func(sseFrame) error) error {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), maxSSEEventBytes)
	var event string
	var data []string
	flush := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		frame := sseFrame{Event: event, Data: strings.Join(data, "\n")}
		event, data = "", data[:0]
		return emit(frame)
	}
	for scan.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scan.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			field, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scan.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return errors.New("provider stream event exceeds size limit")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errStreamInterrupted
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(data) > 0 {
		return errStreamInterrupted
	}
	return nil
}
