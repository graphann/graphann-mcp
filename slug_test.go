package main

import "testing"

func TestSlugify(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"", "default"},
		{"   ", "default"},
		{"MemoryPalace", "memorypalace"},
		{"Memory Palace", "memory-palace"},
		{"my_project.v2", "my-project-v2"},
		{"--weird--name--", "weird-name"},
		{"lukasz-claude", "lukasz-claude"},
		{"/Users/nvm/Documents/projects/memory-palace", "users-nvm-documents-projects-memory-palace"},
	}
	for _, c := range cases {
		if got := slugify(c.in); got != c.out {
			t.Errorf("slugify(%q) = %q, want %q", c.in, got, c.out)
		}
	}
	// cap at 64 chars
	long := "abcdefghij" // 10 chars, repeat 10x = 100
	for i := 0; i < 9; i++ {
		long += "abcdefghij"
	}
	if got := slugify(long); len(got) > 64 {
		t.Errorf("slugify should cap at 64, got len %d", len(got))
	}
}

func TestEncodeMemoryText(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		tags   []string
		source string
		text   string
		want   string
	}{
		{"plain", "", nil, "", "hello world", "hello world"},
		{"kind only", "preference", nil, "", "likes Go", "[kind:preference] likes Go"},
		{"tags only", "", []string{"go", "infra"}, "", "uses Go", "[tags:go,infra] uses Go"},
		{"all", "fact", []string{"Go", "Network"}, "conversation", "port 38888", "[kind:fact tags:go,network source:conversation] port 38888"},
		{"empty tags filtered", "fact", []string{"", "   "}, "", "x", "[kind:fact] x"},
	}
	for _, c := range cases {
		if got := encodeMemoryText(c.kind, c.tags, c.source, c.text); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStripTagPrefix(t *testing.T) {
	cases := []struct{ in, out string }{
		{"[kind:x] hello", "hello"},
		{"[kind:x tags:a,b] hello world", "hello world"},
		{"no prefix", "no prefix"},
		{"[  ] body", "[  ] body"},
		{"[WIP] fix later", "[WIP] fix later"},
		{"[kind:x source:a b] hi", "hi"},
		{"[source:x)] hi", "hi"},
	}
	for _, c := range cases {
		if got := stripTagPrefix(c.in); got != c.out {
			t.Errorf("stripTagPrefix(%q) = %q, want %q", c.in, got, c.out)
		}
	}
}
