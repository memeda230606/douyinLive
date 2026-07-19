package capture

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildASRAudioArgs(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "output.wav.partial")
	args, err := buildASRAudioArgs(mediaArtifactPipeInput, output)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-n",
		"-i", mediaArtifactPipeInput,
		"-map", "0:a:0", "-vn", "-ac", "1", "-ar", "16000",
		"-c:a", "pcm_s16le", "-map_metadata", "-1", "-map_chapters", "-1",
		"-f", "wav", output,
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args mismatch: %#v", args)
	}
}

func TestBuildPlaybackMP4Args(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "output.mp4.partial")
	args, err := buildPlaybackMP4Args(mediaArtifactPipeInput, output)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-n",
		"-i", mediaArtifactPipeInput,
		"-map", "0:v:0", "-map", "0:a:0?", "-c", "copy",
		"-map_metadata", "-1", "-map_chapters", "-1",
		"-movflags", "+faststart", "-f", "mp4", output,
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args mismatch: %#v", args)
	}
}

func TestPlaybackCopyCompatible(t *testing.T) {
	tests := []struct {
		video string
		audio string
		want  bool
	}{
		{video: "h264", audio: "aac", want: true},
		{video: " H264 ", audio: "", want: true},
		{video: "h264", audio: "opus", want: false},
		{video: "hevc", audio: "aac", want: false},
		{video: "", audio: "aac", want: false},
	}
	for _, test := range tests {
		if got := playbackCopyCompatible(test.video, test.audio); got != test.want {
			t.Fatalf("compatible(%q,%q) = %v, want %v", test.video, test.audio, got, test.want)
		}
	}
}

func TestArtifactArgsRejectRelativePaths(t *testing.T) {
	if _, err := buildASRAudioArgs("relative.mkv", "relative.wav"); err == nil {
		t.Fatal("expected relative paths to be rejected")
	}
	if _, err := buildPlaybackMP4Args("relative.mkv", "relative.mp4"); err == nil {
		t.Fatal("expected relative paths to be rejected")
	}
}
