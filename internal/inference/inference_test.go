package inference

import (
	"reflect"
	"strings"
	"testing"
)

func fullConfig(engine Engine) Config {
	return Config{
		Engine: engine, ModelID: "meta-llama/Llama-3.1-8B-Instruct",
		ServedModelName: "llama", Quantization: "awq", MaxModelLen: 8192,
		TensorParallel: 2, MemoryFraction: 0.85,
		Flags: []Flag{
			{Name: "--enable-prefix-caching"},
			{Name: "--kv-cache-dtype", Value: "fp8"},
		},
	}
}

// The container input contract (ADR-080 §4): vLLM's official image IS the
// server — flags alone, never a `vllm serve` prefix; SGLang ships no serving
// entrypoint — the full invocation. Each engine renders its own spelling of
// the shared knobs.
func TestContainerCommandContract(t *testing.T) {
	vllm := ContainerCommand(fullConfig(EngineVLLM), "sk-key")
	if vllm[0] != "--model" {
		t.Fatalf("the vLLM container command must be flags alone, got %q first", vllm[0])
	}
	joined := strings.Join(vllm, " ")
	for _, want := range []string{
		"--model meta-llama/Llama-3.1-8B-Instruct", "--host 0.0.0.0", "--port 8000",
		"--api-key sk-key", "--served-model-name llama", "--quantization awq",
		"--max-model-len 8192", "--tensor-parallel-size 2", "--gpu-memory-utilization 0.85",
		"--enable-prefix-caching --kv-cache-dtype fp8",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("vLLM command misses %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "vllm serve") || strings.Contains(joined, "--context-length") {
		t.Fatalf("vLLM command carries another engine's spelling:\n%s", joined)
	}

	sgl := ContainerCommand(fullConfig(EngineSGLang), "sk-key")
	if got := strings.Join(sgl[:3], " "); got != "python3 -m sglang.launch_server" {
		t.Fatalf("the SGLang container command must be the full invocation, got %q", got)
	}
	joined = strings.Join(sgl, " ")
	for _, want := range []string{
		"--model-path meta-llama/Llama-3.1-8B-Instruct", "--context-length 8192",
		"--tp-size 2", "--mem-fraction-static 0.85",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("SGLang command misses %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "--max-model-len") || strings.Contains(joined, "--gpu-memory-utilization") {
		t.Fatalf("SGLang command carries vLLM spellings:\n%s", joined)
	}
}

// Round-trip identity (ADR-080 §3bis): export → import is the identity on
// the configuration, field for field, flag for flag — for both engines. The
// API key is dropped WITH a notice (the platform manages it).
func TestExportImportRoundTrip(t *testing.T) {
	for _, engine := range []Engine{EngineVLLM, EngineSGLang} {
		cfg := fullConfig(engine)
		human := HumanCommand(cfg, "sk-secret")
		res, err := Parse(human)
		if err != nil {
			t.Fatalf("%s: %v", engine, err)
		}
		if !reflect.DeepEqual(res.Config, cfg) {
			t.Fatalf("%s round-trip drifted:\n exported %s\n got %+v\n want %+v", engine, human, res.Config, cfg)
		}
		// The export shows the managed flags (--host/--port/--api-key) so the
		// command runs by hand; the import hands them back to the platform,
		// each with its notice — three drops, none silent.
		if len(res.Notices) != 3 || !strings.Contains(strings.Join(res.Notices, " "), "--api-key") {
			t.Fatalf("%s: managed flags owe their notices, got %v", engine, res.Notices)
		}
	}
}

// The masked export never leaks the key, and quoting survives hostile values.
func TestHumanCommandMasking(t *testing.T) {
	cfg := fullConfig(EngineVLLM)
	masked := HumanCommand(cfg, MaskedKey)
	if strings.Contains(masked, "sk-") || !strings.Contains(masked, "--api-key '"+MaskedKey+"'") {
		t.Fatalf("masked command = %s", masked)
	}
	cfg.Flags = []Flag{{Name: "--chat-template", Value: "/a dir/it's.jinja"}}
	round, err := Parse(HumanCommand(cfg, ""))
	if err != nil || round.Config.Flags[0].Value != "/a dir/it's.jinja" {
		t.Fatalf("hostile value did not survive quoting: %+v, %v", round.Config.Flags, err)
	}
}

func TestParseForeignForms(t *testing.T) {
	t.Run("vLLM positional model, blog style", func(t *testing.T) {
		res, err := Parse("vllm serve org/model --max-model-len 4096 --enforce-eager")
		if err != nil {
			t.Fatal(err)
		}
		if res.Config.Engine != EngineVLLM || res.Config.ModelID != "org/model" ||
			res.Config.MaxModelLen != 4096 {
			t.Fatalf("config = %+v", res.Config)
		}
		if len(res.Config.Flags) != 1 || res.Config.Flags[0].Name != "--enforce-eager" || res.Config.Flags[0].Value != "" {
			t.Fatalf("flags = %+v", res.Config.Flags)
		}
	})

	t.Run("SGLang docker example, reserved flags dropped with notices", func(t *testing.T) {
		res, err := Parse("python3 -m sglang.launch_server --model-path meta-llama/Llama-3.1-8B-Instruct --host 0.0.0.0 --port 30000")
		if err != nil {
			t.Fatal(err)
		}
		if res.Config.Engine != EngineSGLang || res.Config.ModelID != "meta-llama/Llama-3.1-8B-Instruct" {
			t.Fatalf("config = %+v", res.Config)
		}
		if len(res.Notices) != 2 {
			t.Fatalf("notices = %v, want --host and --port dropped", res.Notices)
		}
		if len(res.Config.Flags) != 0 {
			t.Fatalf("reserved flags leaked into tier 2: %+v", res.Config.Flags)
		}
	})

	t.Run("no prefix: the flag spelling decides the engine", func(t *testing.T) {
		res, err := Parse("--model-path x/y --mem-fraction-static 0.7")
		if err != nil {
			t.Fatal(err)
		}
		if res.Config.Engine != EngineSGLang || res.Config.MemoryFraction != 0.7 {
			t.Fatalf("config = %+v", res.Config)
		}
	})

	t.Run("quotes and = forms survive", func(t *testing.T) {
		res, err := Parse(`vllm serve --model org/m --chat-template '/tpl/a b.jinja' --kv-cache-dtype=fp8`)
		if err != nil {
			t.Fatal(err)
		}
		want := []Flag{{Name: "--chat-template", Value: "/tpl/a b.jinja"}, {Name: "--kv-cache-dtype", Value: "fp8"}}
		if !reflect.DeepEqual(res.Config.Flags, want) {
			t.Fatalf("flags = %+v", res.Config.Flags)
		}
	})

	t.Run("refusals name their cause", func(t *testing.T) {
		if _, err := Parse("vllm serve"); err == nil || !strings.Contains(err.Error(), "no model") {
			t.Fatalf("err = %v", err)
		}
		if _, err := Parse(`vllm serve --model "broken`); err == nil || !strings.Contains(err.Error(), "unbalanced quote") {
			t.Fatalf("err = %v", err)
		}
		if _, err := Parse("vllm serve org/m stray-token"); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
			t.Fatalf("err = %v", err)
		}
		if _, err := Parse("vllm serve org/m --max-model-len banana"); err == nil || !strings.Contains(err.Error(), "invalid value") {
			t.Fatalf("err = %v", err)
		}
	})
}

// Tier-2 hygiene (ADR-080 §1): reserved flags and tier-1 spellings are
// refused by name, and non-flags never pass as flags.
func TestValidateFlags(t *testing.T) {
	if err := ValidateFlags([]Flag{{Name: "--enable-prefix-caching"}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Flag{
		{Name: "--api-key", Value: "x"},
		{Name: "--port", Value: "9"},
		{Name: "--model", Value: "y"},
		{Name: "--gpu-memory-utilization", Value: "0.5"},
		{Name: "oops"},
	} {
		if err := ValidateFlags([]Flag{bad}); err == nil {
			t.Fatalf("flag %+v must be refused", bad)
		}
	}
	if len(ReservedFlagNames()) == 0 {
		t.Fatal("the reserved list must be documented")
	}
}
