package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/exp/slices"
	"golang.org/x/term"
)

type State struct {
	sample     []rune
	typedIndex int
	ghostIndex int
	typos      []int
}

type SavedSample struct {
	Text         string `json:"text"`
	CharTimes    []int  `json:"char_times,omitempty"`
	PersonalBest int    `json:"personal_best,omitempty"`
}

var (
	state        State
	stateMu      sync.Mutex
	savedSamples []SavedSample
	hasPb        bool
)

func main() {
	sSamplesFile, err := os.Open("savedSamples.json")
	if err != nil {
		fmt.Println("opening saved samples file", err.Error())
		return
	}

	jsonParser := json.NewDecoder(sSamplesFile)
	if err = jsonParser.Decode(&savedSamples); err != nil {
		fmt.Println("parsing config file", err.Error())
		return
	}
	savedSample := &savedSamples[0]

	sampleRunes := []rune(savedSample.Text)
	state = State{
		sample:     sampleRunes,
		typedIndex: 0,
		ghostIndex: 0,
		typos:      make([]int, 0),
	}

	hasPb = len(savedSample.CharTimes) != 0
	if !hasPb {
		savedSample.CharTimes = make([]int, len(state.sample))
	}
	// clean sreen, place cursor
	fmt.Print("\033[2J\033[H")

	// raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error enabling raw mode", err)
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	go renderLoop()

	var start time.Time
	var inputBuf []byte
	currentCharTime := time.Now()
	var timeDifChars time.Duration = 0
	currentCharTimes := make([]int, len(savedSample.CharTimes))
	copy(currentCharTimes, savedSample.CharTimes)

	for state.typedIndex < len(state.sample) {
		b := make([]byte, 1)
		_, err := os.Stdin.Read(b)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error reading input", err)
			break
		}
		inputBuf = append(inputBuf, b[0])

		r, size := utf8.DecodeRune(inputBuf)
		if r == utf8.RuneError && size == 1 {
			continue
		}
		inputBuf = inputBuf[size:]

		stateMu.Lock()

		if state.typedIndex == 0 {
			if hasPb {
				go ghostAnimation()
			}
			start = time.Now()
		}

		if r == state.sample[state.typedIndex] {
			//correct a typo
			if slices.Contains(state.typos, state.typedIndex) {
				idx := slices.Index(state.typos, state.typedIndex)
				state.typos = slices.Delete(state.typos, idx, idx+1)
			}
			if state.typedIndex == 0 {
				currentCharTime = time.Now()
			}
			if len(state.typos) == 0 {
				timeDifChars = time.Since(currentCharTime)
				currentCharTimes[state.typedIndex] = int(timeDifChars.Milliseconds())
				currentCharTime = time.Now()
			}
			state.typedIndex++
		} else if r == 127 { // Backspace
			if state.typedIndex > 0 {
				state.typedIndex--
			}
		} else if r == 23 { // ctrl + backspace (erase word)
			if state.typedIndex > 0 {
				for ok := true; ok; ok = (state.typedIndex > 0 && state.sample[state.typedIndex] != ' ') { //like java 'do while'
					fmt.Printf("\033[1D")
					state.typedIndex--
				}
			}
		} else if r == 8 { // Ctrl + Shift + Backspace (erase all)
			for state.typedIndex > 0 {
				state.typedIndex--
			}
		} else if r == 3 { // Ctrl + C (quit program)
			fmt.Print("\033[2J\033[H")
			stateMu.Unlock()
			return
		} else if r == 13 || r == 10 { //new line
			if state.sample[state.typedIndex] == '\n' {
				if slices.Contains(state.typos, state.typedIndex) {
					idx := slices.Index(state.typos, state.typedIndex)
					state.typos = slices.Delete(state.typos, idx, idx+1)
				}
				if state.typedIndex == 0 {
					currentCharTime = time.Now()
				}
				if len(state.typos) == 0 {
					timeDifChars = time.Since(currentCharTime)
					currentCharTimes[state.typedIndex] = int(timeDifChars.Milliseconds())
					currentCharTime = time.Now()
				}
				state.typedIndex++
			} else {
				state.typos = append(state.typos, state.typedIndex)
				state.typedIndex++
			}
		} else {
			state.typos = append(state.typos, state.typedIndex)
			state.typedIndex++
		}
		stateMu.Unlock()
	}

	elapsed := time.Since(start)
	var isPB bool = false
	if len(state.typos) == 0 {
		if !hasPb {
			savedSample.PersonalBest = int(elapsed)
			isPB = true
		} else if int(elapsed) < savedSample.PersonalBest {
			isPB = true
		}
	}

	var highlightColor int
	if isPB {
		highlightColor = 45
		savedSample.PersonalBest = int(elapsed)
		copy(savedSample.CharTimes, currentCharTimes)
	} else if len(state.typos) != 0 {
		highlightColor = 41
	} else {
		highlightColor = 42
	}

	fmt.Print("\n\r")
	wordCount := countWords(state.sample)
	elapsedMinutes := elapsed.Minutes()
	wpm := float64(wordCount) / elapsedMinutes
	fmt.Printf("\033[%dm wpm: %v\033[0m\t", highlightColor, wpm)
	fmt.Printf("\033[%dm Time: %v\033[0m\n\r", highlightColor, elapsed)

	// update json
	sSamplesFile, err = os.OpenFile("savedSamples.json", os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Println("opening saved samples file for writing", err.Error())
		return
	}
	defer sSamplesFile.Close()

	jsonEncoder := json.NewEncoder(sSamplesFile)
	if err = jsonEncoder.Encode(&savedSamples); err != nil {
		fmt.Println("encoding saved samples", err.Error())
		return
	}
}

func renderLoop() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		render()
	}
}

func render() {
	stateMu.Lock()
	sample := state.sample
	typedIndex := state.typedIndex
	ghostIndex := state.ghostIndex
	typos := make([]int, len(state.typos))
	copy(typos, state.typos)
	stateMu.Unlock()

	// clen sreen
	fmt.Print("\033[H\033[2J")
	for i, ch := range sample {
		if ch == '\n' {
			if i < typedIndex {
				if slices.Contains(typos, i) {
					fmt.Printf("\033[41m%c\033[0m", ' ')
				} else {
					fmt.Printf("\033[109m%c\033[0m", ' ')
				}
				fmt.Print("\r\n")
				continue

			} else {

				fmt.Printf("\033[109m%c\033[0m", ' ')
				fmt.Print("\r\n")
				continue
			}
		}

		if i == ghostIndex && hasPb {
			fmt.Printf("\033[95m%c\033[0m", ch)
		} else if i < typedIndex {

			if slices.Contains(typos, i) {
				if ch == '\n' {
					fmt.Printf("\033[41m%c\033[0m", ' ')
				}
				if ch == ' ' {
					fmt.Printf("\033[41m%c\033[0m", ch)
				} else {
					fmt.Printf("\033[91m%c\033[0m", ch)
				}
			} else {
				fmt.Printf("\033[97m%c\033[0m", ch)
			}
		} else {
			fmt.Printf("\033[90m%c\033[0m", ch)
		}
	}

	row, col := 0, 0
	for i := 0; i < typedIndex && i < len(sample); i++ {
		if sample[i] == '\n' {
			row++
			col = 0
		} else {
			col++
		}
	}
	fmt.Printf("\033[%d;%dH", row+1, col+1)
}

func ghostAnimation() {
	i := 0
	for state.ghostIndex < len(state.sample) {
		t := savedSamples[0].CharTimes[i]
		time.Sleep(time.Duration(t) * time.Millisecond)
		i++
		stateMu.Lock()
		state.ghostIndex++
		stateMu.Unlock()
	}
}
func countWords(sample []rune) int {
	inWord := false
	wordCount := 0

	for _, r := range sample {
		if r == ' ' || r == '\n' || r == '\t' {
			if inWord {
				inWord = false
			}
		} else {
			if !inWord {
				inWord = true
				wordCount++
			}
		}
	}

	return wordCount
}
