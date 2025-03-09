package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"
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
	state         State
	stateMu       sync.Mutex
	savedSamples  []SavedSample
	hasPb         bool
	ghostRow      int
	ghostCol      int
	typeRow       int
	typeCol       int
	terminalWidth int
	savedSample   *SavedSample
	oldState      *term.State
)

func main() {
	if err := loadSavedSamples("savedSamples.json"); err != nil {
		fmt.Println("Error:", err)
		return
	}

	savedSample := &savedSamples[0]

	initializeState(savedSample)
	var err error
	oldState, err = setupTerminal()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	ghostRow, ghostCol, typeRow, typeCol = 0, 0, 0, 0

	render(0, "initial")
	var start time.Time
	var inputBuf []byte
	currentCharTime := time.Now()
	var timeDifChars time.Duration = 0
	currentCharTimes := make([]int, len(savedSample.CharTimes))
	copy(currentCharTimes, savedSample.CharTimes)

	setupResizeListener()

	firstTypedChar := true
	for state.typedIndex < len(state.sample) {
		r, err := readRune(&inputBuf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error reading input", err)
			break
		}

		stateMu.Lock()
		if firstTypedChar {
			firstTypedChar = false
			startGhostAnimation()
			start = time.Now()
		}

		handleInput(r, &currentCharTime, &timeDifChars, currentCharTimes)
		stateMu.Unlock()
	}

	elapsed := time.Since(start)
	isPB := updatePersonalBest(elapsed, currentCharTimes)

	displayResults(elapsed, isPB)
	saveSamples("savedSamples.json")
}

func loadSavedSamples(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("opening saved samples file: %w", err)
	}
	defer file.Close()

	jsonParser := json.NewDecoder(file)
	if err = jsonParser.Decode(&savedSamples); err != nil {
		return fmt.Errorf("parsing config file: %w", err)
	}
	return nil
}

func initializeState(savedSample *SavedSample) {
	state = State{
		sample:     []rune(savedSample.Text),
		typedIndex: 0,
		ghostIndex: 0,
		typos:      make([]int, 0),
	}

	hasPb = len(savedSample.CharTimes) != 0
	if !hasPb {
		savedSample.CharTimes = make([]int, len(state.sample))
	}
}

func setupTerminal() (*term.State, error) {
	var err error
	_, terminalWidth, err = getTerminalSize()
	if err != nil {
		return nil, err
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, fmt.Errorf("error enabling raw mode: %w", err)
	}
	return oldState, nil
}

func restoreTerminal(oldState *term.State) {
	term.Restore(int(os.Stdin.Fd()), oldState)
}

func setupResizeListener() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGWINCH)
	go func() {
		for {
			<-sigs
			render(0, "resize")
		}
	}()
}

func readRune(inputBuf *[]byte) (rune, error) {
	b := make([]byte, 1)
	_, err := os.Stdin.Read(b)
	if err != nil {
		return utf8.RuneError, err
	}
	*inputBuf = append(*inputBuf, b[0])

	r, size := utf8.DecodeRune(*inputBuf)
	if r == utf8.RuneError && size == 1 {
		return utf8.RuneError, nil
	}
	*inputBuf = (*inputBuf)[size:]
	return r, nil
}

func startGhostAnimation() {
	if hasPb {
		go func() {
			for newGhostIndex := range ghostAnimation() {
				render(newGhostIndex, "ghost")
			}
		}()
	}
}

func handleInput(r rune, currentCharTime *time.Time, timeDifChars *time.Duration, currentCharTimes []int) {
	switch r {
	case state.sample[state.typedIndex]:
		handleCorrectInput(currentCharTime, timeDifChars, currentCharTimes)
	case 127:
		handleBackspace()
	case 23:
		handleCtrlBackspace()
	case 8:
		handleCtrlShiftBackspace()
	case 3:
		handleCtrlC()
	case 13, 10:
		handleNewLine(currentCharTime, timeDifChars, currentCharTimes)
	case 27: //esc
	default:
		handleTypo()
	}
}

func handleCorrectInput(currentCharTime *time.Time, timeDifChars *time.Duration, currentCharTimes []int) {
	if slices.Contains(state.typos, state.typedIndex) {
		idx := slices.Index(state.typos, state.typedIndex)
		state.typos = slices.Delete(state.typos, idx, idx+1)
	}
	if state.typedIndex == 0 {
		*currentCharTime = time.Now()
	}
	if len(state.typos) == 0 {
		*timeDifChars = time.Since(*currentCharTime)
		currentCharTimes[state.typedIndex] = int(timeDifChars.Milliseconds())
		*currentCharTime = time.Now()
	}
	state.typedIndex++
	render(state.typedIndex, "typedIncreased")
}

func handleBackspace() {
	if state.typedIndex > 0 {
		state.typedIndex--
		render(state.typedIndex, "typedDecreased")
	}
}

func handleCtrlBackspace() {
	if state.typedIndex > 0 {
		for ok := true; ok; ok = (state.typedIndex > 0 && state.sample[state.typedIndex-1] != ' ') {
			state.typedIndex--
			render(state.typedIndex, "typedDecreased")
		}
	}
}

func handleCtrlShiftBackspace() {
	for state.typedIndex > 0 {
		state.typedIndex--
		render(state.typedIndex, "typedDecreased")
	}
}

func handleCtrlC() {
	fmt.Print("\033[2J\033[H")
	term.Restore(int(os.Stdin.Fd()), oldState)
	os.Exit(0)
}

func handleNewLine(currentCharTime *time.Time, timeDifChars *time.Duration, currentCharTimes []int) {
	if state.sample[state.typedIndex] == '\n' {
		if slices.Contains(state.typos, state.typedIndex) {
			idx := slices.Index(state.typos, state.typedIndex)
			state.typos = slices.Delete(state.typos, idx, idx+1)
		}
		if state.typedIndex == 0 {
			*currentCharTime = time.Now()
		}
		if len(state.typos) == 0 {
			*timeDifChars = time.Since(*currentCharTime)
			currentCharTimes[state.typedIndex] = int(timeDifChars.Milliseconds())
			*currentCharTime = time.Now()
		}
		state.typedIndex++
		render(state.typedIndex, "typedIncreased")
	} else {
		state.typos = append(state.typos, state.typedIndex)
		state.typedIndex++
		render(state.typedIndex, "typedIncreased")
	}
}

func handleTypo() {
	if !slices.Contains(state.typos, state.typedIndex) {
		state.typos = append(state.typos, state.typedIndex)
	}
	state.typedIndex++
	render(state.typedIndex, "typedIncreased")
}

func updatePersonalBest(elapsed time.Duration, currentCharTimes []int) bool {
	var isPB bool
	if len(state.typos) == 0 {
		if !hasPb {
			savedSample.PersonalBest = int(elapsed)
			isPB = true
		} else if int(elapsed) < savedSample.PersonalBest {
			isPB = true
		}
	}

	if isPB {
		savedSample.PersonalBest = int(elapsed)
		copy(savedSample.CharTimes, currentCharTimes)
	}
	return isPB
}

func displayResults(elapsed time.Duration, isPB bool) {
	fmt.Print("\033[2J") //clean screen
	fmt.Printf("\033[H") //return home
	wordCount := countWords(state.sample)
	elapsedMinutes := elapsed.Minutes()
	wpm := float64(wordCount) / elapsedMinutes

	var highlightColor int
	if isPB {
		highlightColor = 45
	} else if len(state.typos) != 0 {
		highlightColor = 41
	} else {
		highlightColor = 42
	}

	fmt.Printf("\033[%dm wpm: %v\033[0m\t", highlightColor, wpm)
	fmt.Printf("\033[%dm Time: %v\033[0m\n\r", highlightColor, elapsed)
}

func saveSamples(filename string) {
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Println("opening saved samples file for writing", err.Error())
		return
	}
	defer file.Close()

	jsonEncoder := json.NewEncoder(file)
	if err = jsonEncoder.Encode(&savedSamples); err != nil {
		fmt.Println("encoding saved samples", err.Error())
		return
	}
}

func render(newIndex int, thingToUpdate string) {
	switch thingToUpdate {
	case "initial":
		fmt.Print("\033[2J")                           //clean screen
		fmt.Printf("\033[H")                           //return home
		fmt.Printf("\033[90m%s", string(state.sample)) //prints the whole sample in gray
		fmt.Printf("\033[H")                           //return home
		fmt.Printf("\033[5 q")                         //change cursor to bar

	case "ghost":
		fmt.Printf("\0337")                                       //save typing position
		fmt.Printf("\033[%d;%dH", ghostRow+1, ghostCol+1)         //position in ghost index
		fmt.Printf("\033[95m%c\033[0m", state.sample[newIndex-1]) //write ghost char
		fmt.Printf("\0338")                                       //back to saved typing position

		if ghostCol == terminalWidth-1 {
			ghostCol = 0
			ghostRow++
		} else {
			ghostCol++
		}

	case "typedIncreased":
		ch := state.sample[newIndex-1]
		if !slices.Contains(state.typos, newIndex-1) {
			fmt.Printf("\033[97m%c\033[0m", ch)
		} else {
			if ch == '\n' {
				fmt.Printf("\033[41m%c\033[0m", ' ')
			} else if ch == ' ' {
				fmt.Printf("\033[41m%c\033[0m", ch)
			} else {
				fmt.Printf("\033[91m%c\033[0m", ch)
			}
		}

		if typeCol == terminalWidth-1 {
			typeCol = 0
			typeRow++
			fmt.Printf("\033[%d;%dH", typeRow+1, typeCol+1) //begining next line

		} else {
			typeCol++
		}

	case "typedDecreased":
		if typeCol != 0 {
			fmt.Printf("\033[D")
			fmt.Printf("\033[90m%c\033[0m", state.sample[newIndex])
			fmt.Printf("\033[D")
			typeCol--

		} else if typeRow != 0 {
			typeCol = terminalWidth - 1
			typeRow--
			fmt.Printf("\033[%d;%dH", typeRow+1, typeCol+1) //position in typed index
			fmt.Printf("\033[90m%c\033[0m", state.sample[newIndex])
			fmt.Printf("\033[%d;%dH", typeRow+1, typeCol+1) //position in typed index
		}

	case "resize":
		stateMu.Lock()
		fmt.Print("\033[H\033[2J") //clean and home
		oldTerminalWidth := terminalWidth
		_, terminalWidth, _ = getTerminalSize()
		fmt.Printf("\033[90m%s", string(state.sample))
		typeCellNumber := oldTerminalWidth*typeRow + typeCol
		typeRow = (typeCellNumber / terminalWidth)
		typeCol = (typeCellNumber % terminalWidth)
		fmt.Printf("\033[%d;%dH", typeRow+1, typeCol+1) //position in typed index

		ghostCellNumber := oldTerminalWidth*ghostRow + ghostCol
		ghostRow = (ghostCellNumber / terminalWidth)
		ghostCol = (ghostCellNumber % terminalWidth)

		stateMu.Unlock()
	}
}

func ghostAnimation() <-chan int {
	ghostChan := make(chan int)
	go func() {
		i := 0
		for state.ghostIndex < len(state.sample) {
			t := savedSamples[0].CharTimes[i]
			time.Sleep(time.Duration(t) * time.Millisecond)
			i++
			stateMu.Lock()
			state.ghostIndex++
			ghostChan <- state.ghostIndex
			stateMu.Unlock()
		}
		close(ghostChan)
	}()
	return ghostChan
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

func getTerminalSize() (int, int, error) {
	file := os.Stdin
	fd := int(file.Fd())

	oldState, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return 0, 0, err
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, oldState)

	newState := *oldState
	newState.Lflag &^= unix.ICANON | unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &newState); err != nil {
		return 0, 0, err
	}

	fmt.Print("\x1b[18t")

	reader := bufio.NewReader(file)
	response := make([]byte, 32)
	file.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	n, err := reader.Read(response)
	if err != nil {
		return 0, 0, err
	}

	trimmed := bytes.Trim(response[:n], "\x1b[t")
	parts := strings.Split(string(trimmed), ";")
	if len(parts) < 3 {
		return 0, 0, fmt.Errorf("unexpected response format")
	}

	height, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}

	width, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, err
	}

	return height, width, nil
}
