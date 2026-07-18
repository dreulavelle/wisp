// Package library builds the virtual directory layout that media servers scan.
package library

import (
	"fmt"
	"path"
	"strings"
)

// MoviePath returns the virtual path for a movie shortcut, mirroring the
// naming a Silo/Plex/Jellyfin scanner expects.
func MoviePath(title string, year int, quality, ext string) string {
	folder := fmt.Sprintf("%s (%d)", sanitize(title), year)
	file := fmt.Sprintf("%s (%d) - [%s]%s", sanitize(title), year, quality, ext)
	return path.Join("movies", folder, file)
}

// EpisodePath returns the virtual path for a series episode shortcut.
func EpisodePath(title string, year, season, episode int, quality, ext string) string {
	folder := fmt.Sprintf("%s (%d)", sanitize(title), year)
	seasonDir := fmt.Sprintf("Season %02d", season)
	file := fmt.Sprintf("%s (%d) - S%02dE%02d - [%s]%s", sanitize(title), year, season, episode, quality, ext)
	return path.Join("shows", folder, seasonDir, file)
}

// DetectQuality reads a resolution label from a release filename or title,
// returning "" when none is recognizable. This lets the label reflect the
// stream AIOStreams actually selected rather than a caller's guess.
func DetectQuality(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "2160p"), strings.Contains(s, "4k"), strings.Contains(s, "uhd"):
		return "2160p"
	case strings.Contains(s, "1080p"):
		return "1080p"
	case strings.Contains(s, "720p"):
		return "720p"
	case strings.Contains(s, "480p"):
		return "480p"
	default:
		return ""
	}
}

// Ext picks a media extension from a release filename, defaulting to .mkv.
func Ext(filename string) string {
	filename = strings.ToLower(strings.TrimSpace(filename))
	for _, e := range []string{".mkv", ".mp4", ".m4v", ".avi", ".ts", ".wmv"} {
		if strings.HasSuffix(filename, e) {
			return e
		}
	}
	return ".mkv"
}

// sanitize strips characters that break filesystem paths across platforms.
func sanitize(name string) string {
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", ":", " -", "*", "", "?", "",
		"\"", "'", "<", "", ">", "", "|", "", "\n", " ", "\t", " ",
	)
	return strings.TrimSpace(replacer.Replace(name))
}
