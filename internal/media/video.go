// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package media

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/superseriousbusiness/gotosocial/internal/log"
)

type gtsVideo struct {
	frame     *gtsImage
	duration  float32 // in seconds
	bitrate   uint64
	framerate float32
}

// decodeVideoFrame decodes and returns an image from a single frame in the given video stream.
func decodeVideoFrame(r io.Reader) (*gtsVideo, error) {
	tf, err := os.CreateTemp(os.TempDir(), "gts-video")
	if err != nil {
		return nil, fmt.Errorf("creating temporary file for video processing: %w", err)
	}
	// defer func() {
	// 	os.Remove(tf.Name())
	// }()

	log.Infof(nil, "created temporary file for video processing: %s", tf.Name())

	_, err = io.Copy(tf, r)

	if err != nil {
		return nil, fmt.Errorf("writing video for processing: %w", err)
	}

	prog := "ffprobe"
	args := []string{
		"-select_streams", "v",
		"-show_entries", "stream=r_frame_rate,bit_rate,duration",
		"-of", "json",
		tf.Name(),
	}
	cmd := exec.Command(prog, args...)
	cmd.Stdin = r
	out := bytes.NewBuffer(make([]byte, 0, 2048))
	cmd.Stdout = out
	cmdErrc := make(chan error, 1)
	cmdErrOut, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer cmd.Process.Kill()
	go func() {
		out, err := io.ReadAll(cmdErrOut)
		if err != nil {
			cmdErrc <- err
			return
		}
		cmd.Wait()
		if cmd.ProcessState.Success() {
			cmdErrc <- nil
			return
		}
		cmdErrc <- fmt.Errorf("metadata probe subprocess failed:\n%s", out)
	}()
	select {
	case err := <-cmdErrc:
		if err != nil {
			return nil, err
		}
	case <-time.After(time.Second):
		return nil, fmt.Errorf("timeout during metadata probe process")
	}
	streamInfo := &struct {
		Streams []struct {
			Duration  string `json:"duration"`
			BitRate   string `json:"bit_rate"`
			FrameRate string `json:"r_frame_rate"`
		} `json:"streams"`
	}{}
	if err := json.Unmarshal(out.Bytes(), &streamInfo); err != nil {
		return nil, fmt.Errorf("failed parsing metadata: %w", err)
	}
	if len(streamInfo.Streams) == 0 {
		return nil, fmt.Errorf("media container did not contain any video streams")
	}

	s := streamInfo.Streams[0]
	video := gtsVideo{}

	// duration
	dur, err := strconv.ParseFloat(s.Duration, 32)
	if err != nil {
		return nil, fmt.Errorf("unable to decode video duration with value %s", s.Duration)
	}
	video.duration = float32(dur)

	// bitrate
	br, err := strconv.ParseUint(s.BitRate, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("unable to decode video bitrate with value %s", s.BitRate)
	}
	video.bitrate = br

	// framerate
	frParts := strings.Split(s.FrameRate, "/")
	if len(frParts) != 2 {
		return nil, fmt.Errorf("unable to decode video framerate with value %s", s.FrameRate)
	}
	frCount, err := strconv.Atoi(frParts[0])
	if err != nil {
		return nil, fmt.Errorf("unable to decode video framerate count with value %s", frParts[0])
	}
	frTime, err := strconv.Atoi(frParts[1])
	if err != nil {
		return nil, fmt.Errorf("unable to decode video framerate base with value %s", frParts[0])
	}
	video.framerate = float32(frCount) / float32(frTime)

	frame, err := extractThumbnail(tf.Name())
	if err != nil {
		return nil, fmt.Errorf("extracting thumbnail: %w", err)
	}
	video.frame = frame

	return &video, nil
}

func extractThumbnail(filepath string) (*gtsImage, error) {
	args := []string{
		"-i", filepath,
		"-vf", "thumbnail=n=10",
		"-frames:v", "1",
		"-f", "image2pipe",
		"-c:v", "mjpeg",
		"pipe:1",
	}

	cmd := exec.Command("ffmpeg", args...)
	b := bytes.NewBuffer([]byte{})
	cmd.Stdout = b
	err := cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("extracting thumbnail using ffmpeg: %w", err)
	}
	img, _, err := image.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("decoding generated thumbnail: %w", err)
	}
	return &gtsImage{img}, nil
}
