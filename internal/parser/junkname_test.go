package parser

import "testing"

func TestIsJunkNameRandomStems(t *testing.T) {
	junk := []string{
		"faststart oybfvg54",
		"faststart sx0d7ra6",
		"Qhwfres4nq btc8",
		"tweeload 386yu81n",
		"IMG 9376 MOV",
		"play (12)",
		"EP01",
		"EP 1 wm",
		"6244342652599276537 1750527086707",
		"vpoVditZmV3N nGM",
		"3wsEI0Fl b5lK4I",
		"2j2 uPfNo78T0LN",
		"Sp11xpyyakK r997",
		"ERO5HORT6",
		"ICICIS",
		"wd1",
		"DJ",
		"2",
		"17",
		"wm b4898eaf",
		"9 JK64QLu3by6Z",
	}
	for _, n := range junk {
		if !IsJunkName(n) {
			t.Errorf("IsJunkName(%q) = false, want true", n)
		}
	}
	good := []string{
		"母上攻略",
		"爱情公寓",
		"TheForbiddenFlower",
		"The Forbidden Flower",
		"Cose坠落日记",
		"Squid Game 2",
	}
	for _, n := range good {
		if IsJunkName(n) {
			t.Errorf("IsJunkName(%q) = true, want false", n)
		}
	}
}

func TestParseRejectsRandomFileName(t *testing.T) {
	for _, f := range []string{"faststart_oybfvg54.mp4", "Qhwfres4nq4kbtc8.mp4", "IMG_9376.MOV"} {
		p, err := New().Parse("", f)
		if err == nil && !IsJunkName(p.ShowName) {
			t.Errorf("Parse(%q) => name %q not recognized as junk", f, p.ShowName)
		}
	}
}
