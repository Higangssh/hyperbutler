package main

import (
	"bufio"
	"errors"
	"io"
	"log"

	"github.com/Higangssh/winbox/internal/spaceguard"
)

type event struct {
	b    spaceguard.Budget
	lost bool
}

// readBudgets parses budget lines until the reader fails. Bad lines are logged and
// dropped.
func readBudgets(r io.Reader, send func(event)) error {
	br := bufio.NewReaderSize(r, 2*spaceguard.MaxLineLen)
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) { // discard the rest of an over-long line
				_, err = br.ReadSlice('\n')
			}
			log.Printf("spaceguard: dropped over-long control line")
			continue
		}
		if err != nil {
			return err
		}
		b, perr := spaceguard.ParseBudget(string(line))
		if perr != nil {
			log.Printf("spaceguard: %v", perr)
			continue
		}
		send(event{b: b})
	}
}
