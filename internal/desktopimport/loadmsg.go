package desktopimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Higangssh/hyperbutler/internal/dockerapi"
)

// loadAnswerMax bounds the image load API's answer: a stream of JSON messages, a few
// per image (more with progress, which quiet=1 turns off).
const loadAnswerMax = 4 << 20

// loadMessage is one message of the image load API's answer.
type loadMessage struct {
	Stream      string `json:"stream"`
	Error       string `json:"error"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// parseLoadAnswer reads the image load API's answer: the references and ids it says it
// loaded ("Loaded image: x", "Loaded image ID: sha256:..."), or the error it reports.
// An answer that is cut short, too long or not JSON is an error.
func parseLoadAnswer(r io.Reader) ([]string, error) {
	lr := &io.LimitedReader{R: r, N: loadAnswerMax + 1}
	d := json.NewDecoder(lr)
	var loaded []string
	for {
		var m loadMessage
		err := d.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if lr.N <= 0 {
			return loaded, errors.New("the image load answer is longer than 4 MiB")
		}
		if err != nil {
			return loaded, fmt.Errorf("read the image load answer: %w", err)
		}
		if m.ErrorDetail != nil && m.ErrorDetail.Message != "" {
			return loaded, fmt.Errorf("image load: %s", dockerapi.Clean(m.ErrorDetail.Message, 512))
		}
		if m.Error != "" {
			return loaded, fmt.Errorf("image load: %s", dockerapi.Clean(m.Error, 512))
		}
		for line := range strings.SplitSeq(m.Stream, "\n") {
			line = strings.TrimSpace(line)
			for _, p := range []string{"Loaded image ID: ", "Loaded image: "} {
				if rest, ok := strings.CutPrefix(line, p); ok && rest != "" {
					loaded = append(loaded, dockerapi.Clean(rest, 256))
					break
				}
			}
		}
	}
	return loaded, nil
}
