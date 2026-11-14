package parser

import "testing"

func TestTitleEpisodeRangeFormats(t *testing.T) {
	cases := []struct {
		caption, file, name string
		ep, end             int
	}{

		{"《乡下小子的姐姐  第1-2集》", "竖屏(1).mp4", "乡下小子的姐姐", 1, 2},
		{"《后悔有妻  第1-2集》", "竖屏(1).mp4", "后悔有妻", 1, 2},
		{"《后悔有妻》 第1-2集", "", "后悔有妻", 1, 2},
		{"【后悔有妻 第1-2集】", "", "后悔有妻", 1, 2},
		{"《远山来信 第十一至十二集》", "", "远山来信", 11, 12},
		{"《远山来信 第１－２集》", "", "远山来信", 1, 2},
		{"《遠山來信 第三～四話》", "", "遠山來信", 3, 4},
		{"《远山来信 第1-2合集》", "", "远山来信", 1, 2},
		{"《远山来信 ep03-04》", "", "远山来信", 3, 4},
		{"", "远山来信 第1-2集_watermarked.mp4", "远山来信", 1, 2},

		{"", "【AI短剧】『情趣318』第1-2集原版 (1).mp4", "情趣318", 1, 2},
		{"【快门沦陷/光影交错爱意失控】1-5 合辑", "", "快门沦陷 光影交错爱意失控", 1, 5},
		{"", "私密视角1-7合集.mp4", "私密视角", 1, 7},
		{"【龙女奇遇记】EP1-6合集", "", "龙女奇遇记", 1, 6},
		{"《远山来信》第 1-2 集", "", "远山来信", 1, 2},
		{"〈远山来信 第1-2集〉", "", "远山来信", 1, 2},
	}
	for _, c := range cases {
		t.Run(c.caption+"/"+c.file, func(t *testing.T) {
			got, err := New().Parse(c.caption, c.file)
			if err != nil {
				t.Fatal(err)
			}
			if got.ShowName != c.name || got.NormName != Normalize(c.name) || got.EpisodeNo != c.ep || got.EpEnd != c.end {
				t.Fatalf("got %q (%q), %d-%d; want %q, %d-%d", got.ShowName, got.NormName, got.EpisodeNo, got.EpEnd, c.name, c.ep, c.end)
			}
		})
	}
}

func TestTitleNoiseKeepsMeaningfulEndings(t *testing.T) {
	for _, name := range []string{"书香门第", "后会有期", "人生续集", "我爱我家第二季", "三体2", "1984", "Squid Game 2"} {
		t.Run(name, func(t *testing.T) {
			got, err := New().Parse("《"+name+"》 第3集", "")
			if err != nil || got.ShowName != name {
				t.Fatalf("Parse(%q) = %+v, %v", name, got, err)
			}
			if got := TrimNameNoise(name, false); got != name {
				t.Errorf("TrimNameNoise(%q) = %q", name, got)
			}
		})
	}
	for _, name := range []string{"书香门第", "后会有期", "人生续集", "我爱我家第二季"} {
		if got := NameFromFileName(name + ".mp4"); got != name {
			t.Errorf("NameFromFileName(%q) = %q", name, got)
		}
	}
}

func TestNameNoiseRangeIsIdempotent(t *testing.T) {
	cases := []struct{ input, want string }{
		{"后悔有妻 第1 2集", "后悔有妻"},
		{"乡下小子的姐姐 第1-2集", "乡下小子的姐姐"},
		{"远山来信 第十一至十二集", "远山来信"},
		{"远山来信 ep03-04", "远山来信"},
		{"远山来信 第１－２集", "远山来信"},
		{"远山来信 第1-2集 水印", "远山来信"},

		{"书香门第", "书香门第"},
		{"不完整标题 第", "不完整标题 第"},
	}
	for _, c := range cases {
		got := TrimNameNoise(c.input, false)
		if got != c.want {
			t.Errorf("TrimNameNoise(%q) = %q, want %q", c.input, got, c.want)
		}
		if twice := TrimNameNoise(got, false); twice != got {
			t.Errorf("not idempotent: %q -> %q -> %q", c.input, got, twice)
		}
	}
}

func TestActualEpisodeAfterSameKindProgress(t *testing.T) {
	cases := []struct {
		text    string
		ep, end int
	}{
		{"《远山来信》更新至第12集，本次第3集", 3, 3},
		{"《远山来信》更新至第12集，本次第3-4集", 3, 4},
		{"《远山来信》更新至EP12 本次EP03", 3, 3},
		{"《远山来信》更新至EP10-12 本次EP3-4", 3, 4},
	}
	for _, c := range cases {
		got, err := New().Parse(c.text, "")
		if err != nil || got.ShowName != "远山来信" || got.EpisodeNo != c.ep || got.EpEnd != c.end || got.ProgressEp {
			t.Errorf("Parse(%q) = %+v, %v", c.text, got, err)
		}
	}
}

func TestMultiTitleUsesSameNameCleanup(t *testing.T) {
	got := MultiEntries("《远山来信 第1-2集》 《庭院春风》第3集 《书香门第》第4集")
	if len(got) != 3 {
		t.Fatalf("MultiEntries = %+v", got)
	}
	for i, want := range []MultiEntry{{"远山来信", 1}, {"庭院春风", 3}, {"书香门第", 4}} {
		if got[i] != want {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestDialogueQuotesAreNotTitleContainers(t *testing.T) {
	for _, caption := range []string{
		"3招！讓女生接受「射臉上」！ 關鍵教學增加意願\n\n #吃瓜 #內涵",
		"彼が深夜に閉じ込める。\n「君の全てが欲しい」。\n『シナリオ変えて超人』\n壁際で繰り返される",
		"氣質美女，國人博主「Chloe Lin」（林千夜），氣質人妻喪夫後寂寞難耐",
		"一声「龙王妃」｜师姐坦白出身回不去了",
	} {
		got, err := New().Parse(caption, "")
		if err == nil && got.Container {
			t.Errorf("Parse(%q) treated dialogue quotes as a title container: %+v", caption, got)
		}
	}

	if got, err := New().Parse("《远山来信》第2集", ""); err != nil || !got.Container || got.ShowName != "远山来信" {
		t.Fatalf("book title regressed: %+v %v", got, err)
	}
}
