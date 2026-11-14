package download

import (
	"context"
	"os/exec"
)

func extractCover(ctx context.Context, videoPath, coverPath string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-ss", "3",
		"-i", videoPath,
		"-frames:v", "1",
		"-vf", "scale=640:-2",
		coverPath,
	)
	return cmd.Run()
}
