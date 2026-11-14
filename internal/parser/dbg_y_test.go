package parser

import "testing"

func TestDbgY(t *testing.T) {
	for _, s := range []string{
		"《母上攻略》第14-15集 连载中 全系列完整版\n简介：偷妈妈\n#ai #母上攻略",
		"《母上攻略》第16-17集 连载中 全系列完整版\n简介：偷妈妈\n#ai #母上攻略",
		"《母上攻略》第14集 连载中\n#ai #母上攻略",
	} {
		p, err := New().Parse(s, "母上攻略14.mp4")
		t.Logf("in=%.40q err=%v name=%q ep=%d end=%d prog=%v hasTitle=%v tag=%q", s, err, p.ShowName, p.EpisodeNo, p.EpEnd, p.ProgressEp, p.HasTitle, p.TagName)
	}
}
