package server

import "github.com/azrinaldi/gemini-stealth-bridge/internal/gemini"

// mediaAccumulator carries generated media across frames of one turn. The
// decoder exposes structured media on each frame; presentation Markdown is
// appended once, at the terminal frame, by the server that owns the response.
type mediaAccumulator struct {
	images []gemini.GeneratedImage
	videos []gemini.GeneratedVideo
}

func (m *mediaAccumulator) Observe(frame gemini.Frame) {
	if len(frame.GeneratedImages) > 0 {
		m.images = frame.GeneratedImages
	}
	if len(frame.GeneratedVideos) > 0 {
		m.videos = frame.GeneratedVideos
	}
}

func (m *mediaAccumulator) HasMedia() bool {
	return len(m.images) > 0 || len(m.videos) > 0
}

func (m *mediaAccumulator) Images() []gemini.GeneratedImage {
	return m.images
}

func (m *mediaAccumulator) Videos() []gemini.GeneratedVideo {
	return m.videos
}

func (m *mediaAccumulator) AppendMarkdown(text string) string {
	if !m.HasMedia() {
		return text
	}
	return gemini.AppendGeneratedMediaMarkdown(text, m.images, m.videos)
}
