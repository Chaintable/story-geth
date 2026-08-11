package live

import (
	"encoding/json"

	"github.com/Chaintable/pipeline/tracer"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/eth/tracers"
)

// 需要上传3种data
// 1. block
// 2. state diff
// 3. block file

func init() {
	tracers.LiveDirectory.Register("pipeline", NewPipelineTracer)
}

func NewPipelineTracer(cfg json.RawMessage) (*tracing.Hooks, error) {
	t, err := tracer.NewPipelineTracer(cfg)
	if err != nil {
		return nil, err
	}
	return &tracing.Hooks{
		OnBlockchainInit: t.OnBlockchainInit,
		OnClose:          t.OnClose,
		OnBlockStart:     t.OnBlockStart,
		OnTxStart:        t.OnTxStart,
		OnTxEnd:          t.OnTxEnd,
		OnEnter:          t.OnEnter,
		OnExit:           t.OnExit,
		OnLog:            t.OnLog,
		OnOpcode:         t.OnOpcode,
		OnGenesisBlock:   t.OnGenesisBlock,
		OnCommit:         t.OnCommit,
	}, nil
}
