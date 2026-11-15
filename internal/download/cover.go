package download

import (
	"context"
	"os/exec"
)

// extractCover 使用 ffmpeg 从视频第 3 秒抽取一帧作为封面
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
