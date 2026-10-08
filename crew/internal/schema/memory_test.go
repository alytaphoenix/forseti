package schema

import (
	"strings"
	"testing"
)

// ---- P14: crew memory integration ----

const memCrewYAML = `
name: shipcrew
agents:
  - {name: scout, prompt: look}
  - {name: builder, prompt: build, memory: off}
  - {name: reviewer, prompt: review, memory_query: review checklist}
memory: {}
`

func TestMemoryBlockDefaults(t *testing.T) {
	c, err := Load([]byte(memCrewYAML))
	if err != nil {
		t.Fatalf("want valid, got %v", err)
	}
	if c.Memory == nil {
		t.Fatalf("memory block lost")
	}
	if got := c.Memory.NS(c.Name); got != "shipcrew" {
		t.Fatalf("default namespace = %q, want crew name", got)
	}
	if !c.Memory.AutoRecallOn() || !c.Memory.AutoWriteOn() {
		t.Fatalf("present block must default both switches on")
	}
	if c.Memory.RecallK != 3 || c.Memory.WriteMaxChars != 4000 {
		t.Fatalf("defaults not applied: %+v", c.Memory)
	}
	if !c.Agents[1].MemoryOff() || c.Agents[0].MemoryOff() {
		t.Fatalf("per-agent off=%v inherit=%v", c.Agents[1].MemoryOff(), c.Agents[0].MemoryOff())
	}
	if c.Agents[2].MemoryQuery != "review checklist" {
		t.Fatalf("memory_query lost: %q", c.Agents[2].MemoryQuery)
	}
}

func TestMemoryBlockExplicit(t *testing.T) {
	c, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nmemory:\n  namespace: lab-x\n  auto_recall: false\n  recall_k: 5\n  auto_write: false\n  write_max_chars: 900\n"))
	if err != nil {
		t.Fatalf("want valid, got %v", err)
	}
	if c.Memory.NS(c.Name) != "lab-x" || c.Memory.AutoRecallOn() || c.Memory.AutoWriteOn() {
		t.Fatalf("explicit values not honoured: %+v", c.Memory)
	}
	if c.Memory.RecallK != 5 || c.Memory.WriteMaxChars != 900 {
		t.Fatalf("explicit dials lost: %+v", c.Memory)
	}
}

func TestMemoryValidationErrors(t *testing.T) {
	_, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x, memory: sometimes}\n"))
	if err == nil || !strings.Contains(err.Error(), "memory must be on|off") {
		t.Fatalf("want agent memory enum error, got %v", err)
	}
	_, err = Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nmemory: {namespace: BAD NS}\n"))
	if err == nil || !strings.Contains(err.Error(), "memory namespace") {
		t.Fatalf("want namespace error, got %v", err)
	}
	_, err = Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nmemory: {recall_k: -1}\n"))
	if err == nil || !strings.Contains(err.Error(), "recall_k") {
		t.Fatalf("want recall_k error, got %v", err)
	}
}
