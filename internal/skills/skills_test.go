package skills

import "testing"

func TestIsFork(t *testing.T) {
	cases := []struct {
		name string
		meta SkillMeta
		want bool
	}{
		{"empty", SkillMeta{}, false},
		{"mode fork", SkillMeta{Mode: "fork"}, true},
		{"mode inline", SkillMeta{Mode: "inline"}, false},
		{"legacy context fork", SkillMeta{Context: "fork"}, true},
		{"mode wins over context", SkillMeta{Mode: "fork", Context: "inline"}, true},
		{"context wins over mode", SkillMeta{Mode: "inline", Context: "fork"}, true},
		{"other mode value", SkillMeta{Mode: "background"}, false},
	}
	for _, tc := range cases {
		if got := tc.meta.IsFork(); got != tc.want {
			t.Errorf("%s: IsFork() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLoadBounds(t *testing.T) {
	if MaxFileBytes != 256<<10 {
		t.Fatalf("MaxFileBytes = %d, want %d", MaxFileBytes, 256<<10)
	}
	if MaxSkills != 200 {
		t.Fatalf("MaxSkills = %d, want 200", MaxSkills)
	}
}
