package tracers

// decorateNestedTraceResponse formats trace results the way Parity does.
// Docs: https://openethereum.github.io/JSONRPC-trace-module
// Example:
/*
{
  "id": 1,
  "jsonrpc": "2.0",
  "result": {
    "output": "0x",
    "stateDiff": { ... },
    "trace": [ { ... }, ],
    "vmTrace": { ... }
  }
}
*/
func decorateNestedTraceResponse(res interface{}, tracer string) interface{} {
	out := map[string]interface{}{}
	if tracer == "callTracerParity" {
		out["trace"] = res
	} else if tracer == "stateDiffTracer" {
		out["stateDiff"] = res
	} else {
		return res
	}
	return out
}

// decorateResponse applies formatting to trace results if needed.
func decorateResponse(res interface{}, config *TraceConfig) (interface{}, error) {
	if config != nil && config.NestedTraceOutput && config.Tracer != nil {
		return decorateNestedTraceResponse(res, *config.Tracer), nil
	}
	return res, nil
}
