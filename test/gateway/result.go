// Package gateway is Spike G (plan Task 3; spec section 17, row 1, offline
// half): the pinned agentgateway image, run standalone with the canonical
// agentgateway.yaml in front of fakeanthropic. The spike itself is
// gateway_test.go (build tag gateway; `nix develop -c make gateway-spike`).
//
// The values below are change detectors (Global Constraints, S20): typed by
// hand from a reviewed run, never written by the test. The suite fails when
// the pinned gateway stops behaving this way. docs/spikes/phase-1.md section
// G has the run they come from.
package gateway

// RouteType is Spike G's decision: the ai.routes type the canonical config
// gives /v1/messages ("detect" or "messages"). detect is semantically exact;
// messages reorders the body's top-level keys and rejects a lone surrogate.
const RouteType = "detect"

// BodyByteEqual records whether, under RouteType, the body the upstream
// receives is bytes.Equal to the one the client sent. Semantic exactness is
// the gate; this is a recorded fact. detect re-encodes a JSON body: numbers
// go through float64 (1e3 becomes 1000.0, 2.50 becomes 2.5), and escapes that
// need none are written raw (é, \/).
const BodyByteEqual = false

// Observation is what one route type did in the reviewed run.
type Observation struct {
	// BodySemanticExact: the upstream body passes the semantic gate (parsed
	// JSON equal, object keys in the same order, every string byte-equal).
	BodySemanticExact bool
	// BodyByteEqual: the upstream body is bytes.Equal to the client's.
	BodyByteEqual bool
	// StreamExact: the first SSE event reaches the client within 500 ms and
	// the client's bytes equal the upstream's, ping included.
	StreamExact bool
	// CumulativeUsage is the token usage a stream records, by
	// gen_ai_token_type, when message_start carries 1000 input and 800
	// cache-read tokens and message_delta repeats them with 50 output tokens
	// (the API's shape, and fakeanthropic's).
	CumulativeUsage map[string]float64
	// SplitUsage is the same when message_delta carries only the 50 output
	// tokens.
	SplitUsage map[string]float64
	// LoneSurrogateStatus is the status of a request whose body holds a
	// lone surrogate escape, which JSON.stringify writes for a string cut
	// inside a surrogate pair.
	LoneSurrogateStatus int
}

// Observed holds both route types. TestRouteTypes asserts all of it.
var Observed = map[string]Observation{
	// "input" is the whole prompt: input_tokens plus cache reads.
	"detect": {
		BodySemanticExact:   true,
		BodyByteEqual:       false,
		StreamExact:         true,
		CumulativeUsage:     map[string]float64{"input": 1800, "input_cache_read": 800, "output": 50},
		SplitUsage:          map[string]float64{"output": 50}, // message_delta only
		LoneSurrogateStatus: 200,                              // unparsable: forwarded as sent
	},
	"messages": {
		BodySemanticExact:   false, // the top-level keys come out in its struct order
		BodyByteEqual:       false,
		StreamExact:         true,
		CumulativeUsage:     map[string]float64{"input": 1800, "input_cache_read": 800, "output": 50},
		SplitUsage:          map[string]float64{"input": 1800, "input_cache_read": 800, "output": 50},
		LoneSurrogateStatus: 400, // "failed to parse Messages request: unexpected end of hex escape"
	},
}
