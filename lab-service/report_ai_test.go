package main

import "testing"

func TestParseAiSections(t *testing.T) {
	ok, err := parseAiSections(`{"sections":[{"heading":"Вывод","paragraphs":["текст"],"bullets":["а"]}]}`)
	if err != nil || ok == nil || len(ok.Sections) != 1 || ok.Sections[0].Heading != "Вывод" {
		t.Fatalf("valid: %v %+v", err, ok)
	}

	fenced := "```json\n{\"sections\":[{\"heading\":\"H\",\"paragraphs\":[\"p\"]}]}\n```"
	if _, err := parseAiSections(fenced); err != nil {
		t.Fatalf("fenced: %v", err)
	}

	prose := "Вот результат:\n{\"sections\":[{\"heading\":\"H\",\"paragraphs\":[\"p\"]}]}\nГотово."
	if _, err := parseAiSections(prose); err != nil {
		t.Fatalf("prose: %v", err)
	}

	if _, err := parseAiSections("просто текст без json"); err == nil {
		t.Fatal("no json: want error")
	}
	if _, err := parseAiSections(`{"sections":[]}`); err == nil {
		t.Fatal("empty sections: want error")
	}
	if _, err := parseAiSections(`{"foo":1}`); err == nil {
		t.Fatal("no sections key: want error")
	}
}

func TestRenderKeyValues(t *testing.T) {
	got := renderKeyValues(map[string]any{"b": 2.0, "a": "x"})
	if got != "a=x; b=2" {
		t.Fatalf("got %q", got)
	}
}
