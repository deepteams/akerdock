// Package inference renders and parses the serve command of the model
// resource (ADR-080, extended by ADR-083). One renderer is the single source
// of truth: the
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

// Engine is the flag VOCABULARY of a model (ADR-080 §1, ADR-083 §1): the
// enum is born with exactly the two engines the DGX Spark playbooks cover,
// and stays at two — an omni sibling spells every knob the way its engine
// does, so it is a modality of that engine, not a third value here.
type Engine string

// The two engines, exactly as the enum spells them.
const (
	EngineVLLM   Engine = "vllm"
	EngineSGLang Engine = "sglang"
)

// Modality is the second, orthogonal axis (ADR-083): the engine decides the
// flag VOCABULARY, the modality decides which program is started. vLLM-Omni
// is a plugin of vLLM and spells every knob the vLLM way; SGLang-Omni is a
// separate package that spells them the SGLang way.
type Modality string

// The two modalities, exactly as the enum spells them.
const (
	ModalityText Modality = "text"
	ModalityOmni Modality = "omni"
)

// OmniMarker activates vLLM's omni path on the very `vllm serve` its
// official image entrypoints — which is why the vLLM half needs no
// invocation of its own (ADR-083 §2).
const OmniMarker = "--omni"

// runtime is one cell of the (engine, modality) table: everything that
// depends on the PAIR rather than on the engine alone.
type runtime struct {
	// Invocation prefixes the container command; empty means the image's
	// ENTRYPOINT is already the server (ADR-080 §4, never overridden).
	Invocation []string
	// HumanPrefix is what the copy-ready command opens with.
	HumanPrefix string
	// Marker is a flag the invocation needs and the form does not carry as
	// a knob — vLLM's `--omni`. Empty for everything else.
	Marker string
	// HealthPath is the readiness signal. Named per runtime because an omni
	// surface serves /v1/audio/speech, not /v1/chat/completions, and the
	// only endpoint the four agree on is this one (ADR-083 §5).
	HealthPath string
	// ImageRequired refuses a default: no omni runtime has an image the
	// platform could pin (ADR-083 §4).
	ImageRequired bool
}

// runtimes is THE table. The asymmetry between the two omni cells is
// upstream's: vLLM-Omni is activated by a marker on the same server, while
// SGLang-Omni is another program entirely (there is no sglang.launch_server
// in its image).
var runtimes = map[Engine]map[Modality]runtime{
	EngineVLLM: {
		ModalityText: {HumanPrefix: "vllm serve", HealthPath: "/health"},
		ModalityOmni: {HumanPrefix: "vllm serve", Marker: OmniMarker, HealthPath: "/health", ImageRequired: true},
	},
	EngineSGLang: {
		ModalityText: {
			Invocation:  []string{"python3", "-m", "sglang.launch_server"},
			HumanPrefix: "python3 -m sglang.launch_server", HealthPath: "/health",
		},
		ModalityOmni: {
			Invocation:  []string{"sgl-omni", "serve"},
			HumanPrefix: "sgl-omni serve", HealthPath: "/health", ImageRequired: true,
		},
	},
}

// RuntimeFor resolves the pair, defaulting the way the column does: an
// unknown or empty engine reads as vLLM, an empty modality as text.
func RuntimeFor(engine Engine, modality Modality) runtime {
	byModality, ok := runtimes[engine]
	if !ok {
		byModality = runtimes[EngineVLLM]
	}
	rt, ok := byModality[modality]
	if !ok {
		rt = byModality[ModalityText]
	}
	return rt
}

// HealthPath is the readiness endpoint of a configuration — the job asks the
// table rather than carrying a literal (ADR-083 §5).
func HealthPath(cfg Config) string { return RuntimeFor(cfg.Engine, cfg.Modality).HealthPath }

// ImageRequired reports whether this pair refuses to fall back on a default
// image (ADR-083 §4).
func ImageRequired(engine Engine, modality Modality) bool {
	return RuntimeFor(engine, modality).ImageRequired
}

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
	Modality        Modality
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

// markerFlags are invocation markers the form carries as a field: they are
// refused in tier 2 like a tier-1 spelling, and a pasted command carrying
// one sets the field instead of keeping the flag (ADR-083 §3).
var markerFlags = map[string]Modality{
	OmniMarker: ModalityOmni,
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
		if _, ok := markerFlags[f.Name]; ok {
			return fmt.Errorf("flag %s is the modality of the form — set it there, not in the flag list", f.Name)
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
	args := []string{modelFlag(cfg.Engine), cfg.ModelID}
	// The modality marker rides with the model it qualifies, before the
	// platform-managed flags (ADR-083 §2).
	if marker := RuntimeFor(cfg.Engine, cfg.Modality).Marker; marker != "" {
		args = append(args, marker)
	}
	args = append(args, "--host", "0.0.0.0", "--port", strconv.Itoa(ContainerPort))
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

// ContainerCommand is the container input contract of the RUNTIME — the
// pair (engine, modality), ADR-080 §4 as extended by ADR-083 §2. The vLLM
// official image's ENTRYPOINT is `vllm serve`, so the command is the flags
// ALONE in both modalities (a prefix would reach the server as a bogus
// argument, and omni is activated by a marker on that same CLI); the SGLang
// image ships no serving entrypoint and the SGLang-Omni image ships another
// program entirely, so both carry their full invocation. The image's own
// entrypoint is never overridden: the GB10 community builds do their
// environment setup in theirs.
func ContainerCommand(cfg Config, apiKey string) []string {
	args := Args(cfg, apiKey)
	invocation := RuntimeFor(cfg.Engine, cfg.Modality).Invocation
	if len(invocation) == 0 {
		return args
	}
	return append(append([]string{}, invocation...), args...)
}

// HumanCommand is the copy-ready form the dashboard shows and the ecosystem
// trades in (ADR-080 §3bis) — always prefixed with the human invocation,
// which the import strips back off.
func HumanCommand(cfg Config, apiKey string) string {
	args := Args(cfg, apiKey)
	prefix := RuntimeFor(cfg.Engine, cfg.Modality).HumanPrefix
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
// prefix (vllm serve, vllm-omni serve, python3 -m sglang.launch_server,
// sgl-omni serve, the legacy api_server module) decides the runtime when
// present; flag spellings decide the engine otherwise, and the `--omni`
// marker the modality. Tier-1 spellings land on their typed fields, everything else
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
	tokens, res.Config.Engine, res.Config.Modality = stripInvocation(tokens)

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
		if modality, ok := markerFlags[name]; ok {
			// A marker is meaning the form has a field for: it is consumed,
			// not dropped, and never lands in tier 2 (ADR-083 §3).
			res.Config.Modality = modality
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
	if res.Config.Modality == "" {
		res.Config.Modality = ModalityText
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
// reports the runtime it names. An empty modality means the prefix does not
// decide it — vLLM's does not, since omni rides on the same `vllm serve`
// behind the `--omni` marker the loop above consumes.
func stripInvocation(tokens []string) ([]string, Engine, Modality) {
	joined := strings.Join(tokens, " ")
	for _, p := range []struct {
		prefix   string
		engine   Engine
		modality Modality
	}{
		{"vllm serve", EngineVLLM, ""},
		{"python3 -m vllm.entrypoints.openai.api_server", EngineVLLM, ""},
		{"python -m vllm.entrypoints.openai.api_server", EngineVLLM, ""},
		// vLLM-Omni's own CLI: the platform renders the marker form, but the
		// ecosystem trades in both and a paste must not care.
		{"vllm-omni serve", EngineVLLM, ModalityOmni},
		{"python3 -m sglang.launch_server", EngineSGLang, ModalityText},
		{"python -m sglang.launch_server", EngineSGLang, ModalityText},
		{"sgl-omni serve", EngineSGLang, ModalityOmni},
	} {
		if joined == p.prefix || strings.HasPrefix(joined, p.prefix+" ") {
			n := len(strings.Fields(p.prefix))
			return tokens[n:], p.engine, p.modality
		}
	}
	return tokens, "", ""
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

// ReservedFlagNames lists the flags tier 2 refuses by name — the
// platform-managed ones and the invocation markers the form carries as a
// field — sorted, for the API documentation and the UI hint.
func ReservedFlagNames() []string {
	names := make([]string, 0, len(reservedFlags)+len(markerFlags))
	for n := range reservedFlags {
		names = append(names, n)
	}
	for n := range markerFlags {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
