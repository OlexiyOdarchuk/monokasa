package pay

import "testing"

func TestExtractCode(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		want   string
	}{
		{"bare", []string{"abcdefgh"}, "abcdefgh"},
		{"lowercased", []string{"ABCDEFGH"}, "abcdefgh"},
		{"with prefix", []string{"code: aabb2233"}, "aabb2233"},
		{"embedded in sentence", []string{"hello abcdefgh world"}, "abcdefgh"},
		{"second field used when first empty", []string{"", "from john abcdefgh"}, "abcdefgh"},
		{"first 8-char token wins", []string{"xyzpqrst aabbccdd"}, "xyzpqrst"},
		{"first field wins over second", []string{"abcdefgh", "ttttuuuu"}, "abcdefgh"},
		{"no match", []string{"no match here"}, ""},
		{"too short", []string{"abcdefg"}, ""},
		{"too long is rejected", []string{"abcdefghi"}, ""},
		{"digits 0/1/8/9 split the run", []string{"1abc2def"}, ""},
		{"all empty", []string{"", ""}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractCode(c.fields...)
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}
