// Package inference renders and parses the serve command of the model
// resource (ADR-080). One renderer is the single source of truth: the
// deployment's container command, the copy-ready human command of the
// dashboard, and the paste-import all go through it — the export/import
// round-trip is the identity on the configuration, pinned by tests.
package inference

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Engine is the inference runtime (ADR-080 §1): the enum is born with
// exactly the two engines the DGX Spark playbooks cover.
type Engine string

// The two engines, exactly as the enum spells them.
const (
	EngineVLLM   Engine = "vllm"
	EngineSGLang Engine = "sglang"
)

// ContainerPort is where both engines are told to listen (`--host 0.0.0.0
// --port …` are platform-managed flags): one constant, so the publish
// mapping never depends on the engine.
const ContainerPort = 8000

// Flag is one tier-2 entry: an upstream flag preserved verbatim, in order.
// An empty Value is a boolean flag.
type Flag struct {
	Name  string `json:"flag"`
	Value string `json:"value,omitempty"`
}

// Config is the typed model configuration the two tiers assemble into
// (ADR-080 §1). Zero values mean "engine default" and render nothing —
// except ModelID and Engine, which are mandatory.
type Config struct {
	Engine          Engine
	ModelID         string
	ServedModelName string
	Quantization    string
	MaxModelLen     int
	TensorParallel  int
	MemoryFraction  float64
	Flags           []Flag
}

// MaskedKey is what the export shows in place of the API key unless the
// caller holds models:credentials (ADR-080 §3bis).
const MaskedKey = "****"

// reservedFlags are managed by the platform (or mapped onto tier-1 fields):
// they may not appear in tier 2, and the import strips the managed ones with
// a notice rather than silently honouring a value that would desynchronize
// the platform's own (ADR-080 §1).
var reservedFlags = map[string]string{
	"--host":         "the platform binds the listen address",
	"--port":         "the platform assigns the port",
	"--api-key":      "the platform manages the endpoint key (models:credentials)",
	"--download-dir": "the platform mounts the server-scoped HF cache",
	"--hf-token":     "the token rides the secret variables, never argv (INV-003)",
}

// tier1Flags maps every spelling of a typed knob onto its Config field, so
// the import lands them in the form rather than in tier 2.
var tier1Flags = map[string]string{
	"--model": "model", "--model-path": "model",
	"--served-model-name":      "served",
	"--quantization":           "quant",
	"-q":                       "quant",
	"--max-model-len":          "maxlen",
	"--context-length":         "maxlen",
	"--tensor-parallel-size":   "tp",
	"-tp":                      "tp",
	"--tp-size":                "tp",
	"--gpu-memory-utilization": "memfrac",
	"--mem-fraction-static":    "memfrac",
}

// ReservedFlagError names the refused flag and why — the message the API
// surfaces verbatim on a tier-2 list carrying one.
func ReservedFlagError(name string) error {
	if why, ok := reservedFlags[name]; ok {
		return fmt.Errorf("flag %s is managed by the platform (%s)", name, why)
	}
	return nil
}

// ValidateFlags refuses reserved and tier-1 flags in tier 2 and anything
// that does not look like a flag. Tier-1 spellings are refused too: a flag
// war between the form and the list is a config that lies to one of them.
func ValidateFlags(flags []Flag) error {
	for _, f := range flags {
		if !strings.HasPrefix(f.Name, "-") || f.Name == "-" || f.Name == "--" {
			return fmt.Errorf("%q does not look like an engine flag (expected --like-this)", f.Name)
		}
		if err := ReservedFlagError(f.Name); err != nil {
			return err
		}
		if _, ok := tier1Flags[f.Name]; ok {
			return fmt.Errorf("flag %s is a typed field of the form — set it there, not in the flag list", f.Name)
		}
	}
	return nil
}

// modelFlag is the engine's spelling of the model reference.
func modelFlag(engine Engine) string {
	if engine == EngineSGLang {
		return "--model-path"
	}
	return "--model"
}

// Args renders the argv AFTER the invocation — flags only, deterministic:
// tier 1 in a fixed order, then tier 2 in the operator's order. This is THE
// renderer (ADR-080 §3bis): container, export and diff all read it.
func Args(cfg Config, apiKey string) []string {
	args := []string{
		modelFlag(cfg.Engine), cfg.ModelID,
		"--host", "0.0.0.0", "--port", strconv.Itoa(ContainerPort),
	}
	if apiKey != "" {
		args = append(args, "--api-key", apiKey)
	}
	if cfg.ServedModelName != "" {
		args = append(args, "--served-model-name", cfg.ServedModelName)
	}
	if cfg.Quantization != "" {
		args = append(args, "--quantization", cfg.Quantization)
	}
	if cfg.MaxModelLen > 0 {
		if cfg.Engine == EngineSGLang {
			args = append(args, "--context-length", strconv.Itoa(cfg.MaxModelLen))
		} else {
			args = append(args, "--max-model-len", strconv.Itoa(cfg.MaxModelLen))
		}
	}
	if cfg.TensorParallel > 1 {
		if cfg.Engine == EngineSGLang {
			args = append(args, "--tp-size", strconv.Itoa(cfg.TensorParallel))
		} else {
			args = append(args, "--tensor-parallel-size", strconv.Itoa(cfg.TensorParallel))
		}
	}
	if cfg.MemoryFraction > 0 {
		frac := strconv.FormatFloat(cfg.MemoryFraction, 'f', -1, 64)
		if cfg.Engine == EngineSGLang {
			args = append(args, "--mem-fraction-static", frac)
		} else {
			args = append(args, "--gpu-memory-utilization", frac)
		}
	}
	for _, f := range cfg.Flags {
		args = append(args, f.Name)
		if f.Value != "" {
			args = append(args, f.Value)
		}
	}
	return args
}

// sglangInvocation is the full command the SGLang image needs — it ships no
// serving entrypoint (ADR-080 §4).
var sglangInvocation = []string{"python3", "-m", "sglang.launch_server"}

// ContainerCommand is the per-engine container input contract (ADR-080 §4):
// the vLLM official image's ENTRYPOINT is the server, so the command is the
// flags ALONE — a `vllm serve` prefix would reach the server as a bogus
// argument; the SGLang image ships no serving entrypoint, so the command is
// the full invocation. The image's own entrypoint is never overridden: the
// GB10 community builds do their environment setup in theirs.
func ContainerCommand(cfg Config, apiKey string) []string {
	args := Args(cfg, apiKey)
	if cfg.Engine == EngineSGLang {
		return append(append([]string{}, sglangInvocation...), args...)
	}
	return args
}

// HumanCommand is the copy-ready form the dashboard shows and the ecosystem
// trades in (ADR-080 §3bis) — always prefixed with the human invocation,
// which the import strips back off.
func HumanCommand(cfg Config, apiKey string) string {
	args := Args(cfg, apiKey)
	prefix := "vllm serve"
	if cfg.Engine == EngineSGLang {
		prefix = strings.Join(sglangInvocation, " ")
	}
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, prefix)
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	return strings.Join(quoted, " ")
}

// shellQuote quotes an argument for the human command when it needs it.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$`*?[](){}<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ParseResult is what a pasted command becomes: the two tiers, plus the
// notices the import owes the operator — a reserved flag dropped, an API key
// discarded. Nothing is ever discarded silently (ADR-080 §3bis).
type ParseResult struct {
	Config  Config
	Notices []string
}

// Parse turns a pasted command back into a configuration. The invocation
// prefix (vllm serve, python3 -m sglang.launch_server, the legacy
// api_server module) decides the engine when present; flag spellings decide
// it otherwise. Tier-1 spellings land on their typed fields, everything else
// keeps its order in tier 2, reserved flags drop with a notice.
func Parse(input string) (ParseResult, error) {
	tokens, err := shellSplit(input)
	if err != nil {
		return ParseResult{}, err
	}
	if len(tokens) == 0 {
		return ParseResult{}, fmt.Errorf("empty command")
	}

	var res ParseResult
	tokens, res.Config.Engine = stripInvocation(tokens)

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "-") {
			// vLLM's positional model form (`vllm serve org/model`).
			if res.Config.ModelID == "" {
				res.Config.ModelID = tok
				continue
			}
			return ParseResult{}, fmt.Errorf("unexpected argument %q — flags look like --this", tok)
		}
		name, value, inline := strings.Cut(tok, "=")
		if !inline && i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
			value = tokens[i+1]
			i++
		}
		if field, ok := tier1Flags[name]; ok {
			if err := res.Config.setTier1(field, name, value); err != nil {
				return ParseResult{}, err
			}
			continue
		}
		if why, ok := reservedFlags[name]; ok {
			res.Notices = append(res.Notices, fmt.Sprintf("%s dropped: %s", name, why))
			continue
		}
		res.Config.Flags = append(res.Config.Flags, Flag{Name: name, Value: value})
	}

	if res.Config.Engine == "" {
		res.Config.Engine = guessEngine(res.Config)
	}
	if res.Config.ModelID == "" {
		return ParseResult{}, fmt.Errorf("no model in the command (--model, --model-path, or vllm serve's positional form)")
	}
	return res, nil
}

// setTier1 lands a tier-1 spelling on its typed field, with the numeric ones
// actually parsed — a paste is a form fill, and a form refuses garbage.
func (c *Config) setTier1(field, name, value string) error {
	fail := func() error { return fmt.Errorf("%s: invalid value %q", name, value) }
	switch field {
	case "model":
		c.ModelID = value
		if strings.HasSuffix(name, "-path") && c.Engine == "" {
			c.Engine = EngineSGLang
		}
	case "served":
		c.ServedModelName = value
	case "quant":
		c.Quantization = value
	case "maxlen":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fail()
		}
		c.MaxModelLen = n
	case "tp":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fail()
		}
		c.TensorParallel = n
	case "memfrac":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f <= 0 || f > 1 {
			return fail()
		}
		c.MemoryFraction = f
		if c.Engine == "" {
			if name == "--mem-fraction-static" {
				c.Engine = EngineSGLang
			} else {
				c.Engine = EngineVLLM
			}
		}
	}
	return nil
}

// stripInvocation removes a recognized human/legacy invocation prefix and
// reports the engine it names.
func stripInvocation(tokens []string) ([]string, Engine) {
	joined := strings.Join(tokens, " ")
	for _, p := range []struct {
		prefix string
		engine Engine
	}{
		{"vllm serve", EngineVLLM},
		{"python3 -m vllm.entrypoints.openai.api_server", EngineVLLM},
		{"python -m vllm.entrypoints.openai.api_server", EngineVLLM},
		{"python3 -m sglang.launch_server", EngineSGLang},
		{"python -m sglang.launch_server", EngineSGLang},
	} {
		if joined == p.prefix || strings.HasPrefix(joined, p.prefix+" ") {
			n := len(strings.Fields(p.prefix))
			return tokens[n:], p.engine
		}
	}
	return tokens, ""
}

// guessEngine decides from flag spellings when no invocation named one; the
// spellings unique to SGLang win, vLLM is the ecosystem default otherwise.
func guessEngine(cfg Config) Engine {
	for _, f := range cfg.Flags {
		if strings.HasPrefix(f.Name, "--schedule-") || f.Name == "--chunked-prefill-size" {
			return EngineSGLang
		}
	}
	return EngineVLLM
}

// shellSplit tokenizes a command line with POSIX-ish quoting — enough for
// the commands this ecosystem trades in (no substitution, on purpose).
func shellSplit(s string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	inToken := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			quote := c
			inToken = true
			i++
			for ; i < len(s) && s[i] != quote; i++ {
				current.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unbalanced quote in the command")
			}
		case c == '\\' && i+1 < len(s):
			inToken = true
			i++
			current.WriteByte(s[i])
		case c == ' ' || c == '\t' || c == '\n':
			if inToken {
				tokens = append(tokens, current.String())
				current.Reset()
				inToken = false
			}
		default:
			inToken = true
			current.WriteByte(c)
		}
	}
	if inToken {
		tokens = append(tokens, current.String())
	}
	return tokens, nil
}

// ReservedFlagNames lists the platform-managed flags, sorted — for the API
// documentation and the UI hint.
func ReservedFlagNames() []string {
	names := make([]string, 0, len(reservedFlags))
	for n := range reservedFlags {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
