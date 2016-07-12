/*
Copyright (c) 2016, Maxim Konakov
All rights reserved.

Redistribution and use in source and binary forms, with or without modification,
are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice,
   this list of conditions and the following disclaimer.
2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.
3. Neither the name of the copyright holder nor the names of its contributors
   may be used to endorse or promote products derived from this software without
   specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED.
IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT,
INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING,
BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY
OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING
NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE,
EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
*/

package main

import (
	"bytes"
	"container/heap"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

const usageFmt = `Usage: %s [OPTION]... FILE
Extract text from scanned pdf document FILE; output directed to stdout.

Options:
  -first n        first page number (optional, default: 1)
  -last  n        last page number (optional, default: last page of the document)
  -filter FILE    filter specification file name (optional, may be given multile times)
  -lang  xxx      document language (optional, default: eng)
`

var firstPage, lastPage uint
var inputFileName, language string
var filterSpecs filterNames

func main() {
	// command line parameters
	flag.Usage = usage
	flag.UintVar(&firstPage, "first", 1, "")
	flag.UintVar(&lastPage, "last", 0, "")
	flag.StringVar(&language, "lang", "eng", "")
	flag.Var(&filterSpecs, "filter", "")
	flag.Parse()

	switch flag.NArg() {
	case 0:
		die("Input file is not specified")
	case 1:
		inputFileName = flag.Arg(0)
	default:
		die("Too many input files")
	}

	// read filters
	lineFilter, textFilter, err := makeFilters()

	if err != nil {
		die(err.Error())
	}

	// OCR
	var text bytes.Buffer

	if err = extractText(&text, lineFilter); err != nil {
		die(err.Error())
	}

	// apply full-text filter
	if _, err = os.Stdout.Write(textFilter(text.Bytes())); err != nil {
		die(err.Error())
	}
}

func extractText(text *bytes.Buffer, filter func([]byte) []byte) (err error) {
	// temporary directory
	var dir string

	dir, err = ioutil.TempDir("", "ocr-")
	if err != nil {
		return
	}

	dir = filepath.FromSlash(dir + "/") // make sure we have trailing slash
	defer os.RemoveAll(dir)

	// signal processing
	signals := make(chan os.Signal, 5)

	go func() {
		<-signals
		os.RemoveAll(dir)
		die("Interrupted")
	}()

	signal.Notify(signals, os.Interrupt, os.Kill)

	// extract images from input file
	if err = extractImages(dir); err != nil {
		return
	}

	// OCR
	return ocr(dir, text, filter)
}

// 'pdfimages' driver
func extractImages(dir string) error {
	args := []string{"-tiff", "-f", strconv.Itoa(int(firstPage))}

	if lastPage >= firstPage {
		args = append(args, "-l", strconv.Itoa(int(lastPage)))
	}

	args = append(args, inputFileName, dir)

	var msg bytes.Buffer

	cmd := exec.Command("pdfimages", args...)
	cmd.Stderr = &msg
	err := cmd.Run()

	if err != nil {
		if _, ok := err.(*exec.ExitError); ok && msg.Len() > 0 {
			err = errors.New(msg.String())
		}
	}

	return err
}

// request/response data structures for parallel ocr
type ocrRequest struct {
	no    uint
	image string
}

func (req *ocrRequest) process() (text []byte, err error) {
	text, err = exec.Command("tesseract", req.image, "-", "-l", language).Output()

	if err != nil {
		msg := fmt.Sprintf("(page %d) ", req.no+firstPage)

		if e, ok := err.(*exec.ExitError); ok {
			if n := bytes.IndexByte(e.Stderr, '\n'); n >= 0 { // get first line only
				e.Stderr = e.Stderr[:n]
			}

			msg += string(bytes.TrimSpace(e.Stderr))
		} else {
			msg += err.Error()
		}

		err = errors.New(msg)
	}

	return
}

type ocrResult struct {
	req  ocrRequest
	err  error
	text []byte
}

func processOCRRequest(req *ocrRequest) (r *ocrResult) {
	r = &ocrResult{req: *req}
	r.text, r.err = req.process()
	return
}

// heap of ocrResult structures for restoring the original page order
type resultHeap []*ocrResult

func (h resultHeap) Len() int           { return len(h) }
func (h resultHeap) Less(i, j int) bool { return h[i].req.no < h[j].req.no }
func (h resultHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *resultHeap) Push(x interface{}) { *h = append(*h, x.(*ocrResult)) }

func (h *resultHeap) Pop() interface{} {
	n := len(*h) - 1
	val := (*h)[n]
	*h = (*h)[:n]
	return val
}

// OCR driver
func ocr(dir string, text *bytes.Buffer, filter func([]byte) []byte) error {
	// list all image files
	files, err := filepath.Glob(dir + "*.tif")
	if err != nil {
		return err
	}

	if len(files) == 0 {
		return errors.New("No images found in file " + inputFileName)
	}

	if len(files) > 1 {
		sort.Strings(files)
	}

	// channels
	n := runtime.NumCPU()
	results := make(chan *ocrResult, n)
	requests := make(chan *ocrRequest, len(files))
	var wg sync.WaitGroup

	// workers
	for i := 0; i < n; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for req := range requests {
				results <- processOCRRequest(req)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// fill in request channel
	for i, file := range files {
		requests <- &ocrRequest{uint(i), file}
	}

	close(requests)

	// read results
	var h resultHeap
	i := uint(0)

	heap.Init(&h)

	for r := range results {
		heap.Push(&h, r)

		for ; len(h) > 0 && h[0].req.no == i; i++ {
			r = heap.Pop(&h).(*ocrResult)

			if r.err != nil {
				return r.err
			}

			// process the result
			reader := bytes.NewBuffer(r.text)

			for s, _ := reader.ReadBytes('\n'); len(s) > 0; s, _ = reader.ReadBytes('\n') {
				if _, err := text.Write(filter(bytes.TrimRightFunc(s, unicode.IsSpace))); err != nil {
					return err
				}

				if err := text.WriteByte('\n'); err != nil {
					return err
				}
			}
		}
	}

	if h.Len() > 0 {
		panic(fmt.Sprintf("Heap still has %d elements", h.Len()))
	}

	return nil
}

// little helpers
func die(msg string) {
	fmt.Fprintln(os.Stderr, "ERROR:", msg)
	os.Exit(1)
}

func usage() {
	fmt.Fprintf(os.Stderr, usageFmt, filepath.Base(os.Args[0]))
}

// fiter function maker
func makeFilters() (lineFilter, textFilter func([]byte) []byte, err error) {
	rules := new(ruleList)

	for _, name := range filterSpecs {
		var file *os.File

		if file, err = os.Open(name); err != nil {
			return
		}

		defer file.Close()

		if err = rules.add(file, name); err != nil {
			return
		}
	}

	lineFilter = seqFilter(rules.lineRules)
	textFilter = seqFilter(rules.textRules)
	return
}

func seqFilter(rules []func([]byte) []byte) func([]byte) []byte {
	if len(rules) == 0 {
		return func(s []byte) []byte { return s }
	}

	return func(s []byte) []byte {
		if len(s) > 0 {
			for _, f := range rules {
				if s = f(s); len(s) == 0 {
					break
				}
			}
		}

		return s
	}
}

// 'filter' command line flag
type filterNames []string

func (flags filterNames) String() string {
	return strings.Join(flags, " ")
}

func (flags *filterNames) Set(val string) error {
	if info, err := os.Stat(val); err != nil {
		if os.IsNotExist(err) {
			err = errors.New("file not found")
		}

		return err
	} else if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}

	*flags = append(*flags, val)
	return nil
}
