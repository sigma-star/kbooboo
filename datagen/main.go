// SPDX-License-Identifier: GPL-2.0-only
/*
 * Copyright (C) 2026 Richard Weinberger <richard@nod.at>
 *
 * Basic mode of operation
 * =======================
 *
 * 1. Scan the MAINTAINERS file and extract maintainer entries.
 *
 * 2. Scan the kernel build directory for object files (*.o). Usually, each object file belongs
 * to very few C files, which all belong to the same driver and subsystem. Hence, they share
 * a common maintainer entry.
 *
 * 3. For each found object file, extract compilation units (names of C sources) and symbol
 * names of type function. Inlined functions from header files have to be ignored, though.
 * This provides a symbol-to-source mapping.
 *
 * 4. Using the symbol-to-source mapping, the file patterns of each maintainer entry can be
 * evaluated. This produces the final symbol-to-maintainer mapping.
 *
 * 5. Symbols, maintainer entries, and the SHA-256 hash of the MAINTAINERS file are stored in a
 * gzip-compressed JSON file, which can be easily queried by the frontend.
 *
 * Usage
 * =====
 *
 * 1. Build the Linux kernel with allmodconfig and ensure CONFIG_DEBUG_INFO_DWARF5 is enabled.
 * 2. Run this tool: e.g., ./datagen --source ~/linux --build /scratch/rw/kbuild
 * 3. Copy the resulting data.json.gz file to a location where the frontend can fetch it.
 *
 * Note: It is possible to combine multiple builds of the same kernel into a single
 * data.json.gz file. This is useful for supporting multiple architectures.
 * Simply run datagen again on a different build.
 * e.g.,
 * $ ./datagen --source ~/linux --build /scratch/rw/kbuild
 * $ ./datagen --source ~/linux --build /scratch/rw/kbuild-arm
 * $ ./datagen --source ~/linux --build /scratch/rw/kbuild-arm64
 *
 * As of Linux 7.0-rc3 a combined data.json.gz of allmodconfig(x86_64), defconfig(arm64) and
 * multi_v7_defconfig(arm) has a size of 5.7MiB.
 */

package main

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"debug/dwarf"
	"debug/elf"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/bmatcuk/doublestar/v4"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
)

type MaintainerEntry struct {
	Name             string
	id               int
	MLContacts       []string `json:",omitempty"`
	PersonContacts   []string `json:",omitempty"`
	ChatContacts     []string `json:",omitempty"`
	WebContacts      []string `json:",omitempty"`
	BugContacts      []string `json:",omitempty"`
	regexPatterns    []string
	filePatterns     []string
	fileAntiPatterns []string
}

type OEntry struct {
	Sources []string
	Symbols []string
	MtIds   []int
	Origin  string
}

type KContactData struct {
	MtHash      string
	Maintainers []MaintainerEntry
	Symbols     map[string][]int
}

var ignoredFunPrefixes = []string{"__pfx__", "__SCT__", "__probestub_", "__traceiter_", "__bpf_trace_"}

var srcPathArg = flag.String("source", ".", "Linux kernel source directory")
var buildPathArg = flag.String("build", "", "Linux kernel build directory")
var dataFileArg = flag.String("data", "data.json.gz", "Data file to work on")

func main() {
	flag.Parse()

	if *srcPathArg == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s --source /path/to/linux [ OPTIONS ]\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}

	if *buildPathArg == "" {
		buildPathArg = srcPathArg
	}

	srcPath, _ := filepath.Abs(*srcPathArg)
	buildPath, _ := filepath.Abs(*buildPathArg)
	dataFile, _ := filepath.Abs(*dataFileArg)

	if _, err := os.Stat(path.Join(srcPath, "kernel/panic.c")); err != nil {
		log.Fatalf("%s does not look like a valid Linux kernel source directory\n", srcPath)
	}

	if _, err := os.Stat(path.Join(buildPath, "kernel/panic.o")); err != nil {
		log.Fatalf("%s does not look like a valid Linux kernel build directory\n", buildPath)
	}

	if !checkForDWARF(path.Join(buildPath, "kernel/panic.o")) {
		log.Fatal("Debug info missing, did you build with CONFIG_DEBUG_INFO_DWARF5 enabled?")
	}

	updating := false
	var kcData KContactData
	if _, err := os.Stat(dataFile); err == nil {
		kcData = readJSON(dataFile)
		updating = true
		log.Printf("Updating %s\n", dataFile)
	}

	log.Println("Scanning MAINTAINERS")

	mtEntries, mtHash, err := parseMaintainersFile(path.Join(srcPath, "MAINTAINERS"))
	if err != nil {
		log.Fatalf("Failed to parse maintainers file: %v", err)
	}

	if updating && mtHash != kcData.MtHash {
		log.Fatalf("%s was created with a different MAINTAINERS file. Aborting.\n", dataFile)
	}
	kcData.MtHash = mtHash

	log.Printf("Found %d MAINTAINERS entries\n", len(mtEntries))

	log.Println("Scanning objects")
	oEntries, err := scanKernelObjs(buildPath)
	if err != nil {
		log.Fatalf("Failed to scan kernel objects: %v", err)
	}
	log.Printf("Found %d objects\n", len(oEntries))

	log.Println("Matching sources to objects")

	matchKernelObjs(oEntries, mtEntries, srcPath)

	n := 0
	for _, e := range oEntries {
		if e.MtIds != nil {
			n++
		}
	}

	r := (100.0 / float64(len(oEntries))) * float64(n)
	log.Printf("Object match rate %.2f%%\n", r)

	if !updating {
		kcData.Maintainers = make([]MaintainerEntry, len(mtEntries))
		kcData.Symbols = make(map[string][]int)

		for _, mte := range mtEntries {
			kcData.Maintainers[mte.id] = mte
		}
	}

	symCount := len(kcData.Symbols)

	for _, oe := range oEntries {
		for _, s := range oe.Symbols {
			newMtIds := append(kcData.Symbols[s], oe.MtIds...)
			if len(newMtIds) > 0 {
				slices.Sort(newMtIds)
				kcData.Symbols[s] = slices.Compact(newMtIds)
			}
		}
	}

	if updating {
		log.Printf("Found %d new symbols\n", len(kcData.Symbols)-symCount)
	}

	writeJSON(dataFile, kcData)
	log.Printf("Wrote %s\n", dataFile)
}

func readJSON(filePath string) KContactData {
	file, err := os.Open(filePath)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		log.Fatal(err)
	}
	defer gzReader.Close()

	var data KContactData
	decoder := json.NewDecoder(gzReader)
	if err := decoder.Decode(&data); err != nil {
		log.Fatal(err)
	}

	return data
}

func writeJSON(filePath string, data interface{}) {
	f, err := os.Create(filePath)
	if err != nil {
		log.Fatalf("Error creating file %s: %v\n", filePath, err)
	}

	defer f.Close()

	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		log.Fatalf("Error creating file %s: %v\n", filePath, err)
	}

	defer gz.Close()

	writer := bufio.NewWriter(gz)
	defer writer.Flush()

	encoder := json.NewEncoder(writer)
	encoder.Encode(data)
}

func checkForDWARF(filePath string) bool {
	f, err := elf.Open(filePath)
	if err != nil {
		return false
	}

	defer f.Close()

	_, err = f.DWARF()
	if err != nil {
		return false
	}

	return true
}

func getSrcAndInlines(filePath string) ([]string, []string) {
	f, err := elf.Open(filePath)
	if err != nil {
		log.Printf("Error opening ELF file: %v\n", err)
		return nil, nil
	}

	defer f.Close()

	dwarfData, err := f.DWARF()
	if err != nil {
		return nil, nil
	}

	reader := dwarfData.Reader()

	var sources []string
	var staticInlineFns []string
	var lineReader *dwarf.LineReader

	for {
		entry, err := reader.Next()
		if err != nil {
			log.Printf("Error reading DWARF entry: %v, skipping it\n", err)
			continue
		}

		if entry == nil {
			break
		}

		if entry.Tag == dwarf.TagCompileUnit {
			cuName, _ := entry.Val(dwarf.AttrName).(string)
			if strings.HasSuffix(cuName, ".c") {
				sources = append(sources, cuName)
			}
			lineReader, _ = dwarfData.LineReader(entry)
		} else if entry.Tag == dwarf.TagSubprogram {
			fnName, _ := entry.Val(dwarf.AttrName).(string)

			if fnName == ""{
				continue
			}

			if val, ok := entry.Val(dwarf.AttrInline).(int64); ok && val == 0 {
				continue
			}

			if fileIdx, ok := entry.Val(dwarf.AttrDeclFile).(int64); ok {
				if lineReader != nil {
					files := lineReader.Files()
					if fileIdx > 0 && int(fileIdx) < len(files) {
						filename := files[fileIdx].Name
						if strings.HasSuffix(filename, ".h") {
							staticInlineFns = append(staticInlineFns, fnName)
						}
					}
				}
			}
		}
	}

	return sources, staticInlineFns
}

func matchFilePattern(src string, pattern string) bool {
	if strings.HasSuffix(pattern, "/") && !strings.Contains(pattern, "*") {
		pattern = pattern + "**"
	}

	matched, err := doublestar.Match(pattern, src)
	if err != nil {
		log.Printf("pattern error %s: %v\n", pattern, err)
	}

	return matched
}

func ignoredFile(src string) bool {
	if strings.HasSuffix(src, ".mod.c") {
		return true
	}

	if src == "scripts/module-common.c" {
		return true
	}

	return false
}

func matchMTEntry(mts []MaintainerEntry, srcs []string, srcPath string) []int {
	var mtIds []int

	for _, src := range srcs {
		src, _ = strings.CutPrefix(src, srcPath+"/")
		if ignoredFile(src) {
			continue
		}

	mte_loop:
		for _, mte := range mts {
			for _, antiPattern := range mte.fileAntiPatterns {
				if matchFilePattern(src, antiPattern) {
					continue mte_loop
				}
			}

			for _, pattern := range mte.regexPatterns {
				re, err := regexp.Compile(pattern)
				if err != nil {
					log.Printf("%s is not a vaild regex: %v\n", pattern, err)
				}

				if re.MatchString(src) {
					mtIds = append(mtIds, mte.id)

					continue mte_loop
				}
			}

			for _, pattern := range mte.filePatterns {
				matched := matchFilePattern(src, pattern)

				if matched {
					mtIds = append(mtIds, mte.id)
					continue mte_loop
				}
			}
		}

	}
	slices.Sort(mtIds)
	return slices.Compact(mtIds)
}

func functionIgnored(name string) bool {
	if name == "" {
		return true
	}

	for _, p := range ignoredFunPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}

	return false
}

func getFunSymNames(filePath string) []string {
	f, err := elf.Open(filePath)
	if err != nil {
		log.Printf("Error opening ELF file: %v\n", err)
		return nil
	}

	defer f.Close()

	syms, err := f.Symbols()
	var funcs []string

	for _, sym := range syms {
		if elf.ST_TYPE(sym.Info) == elf.STT_FUNC {
			if elf.ST_BIND(sym.Info) != elf.STB_WEAK && !functionIgnored(sym.Name) {
				funcs = append(funcs, sym.Name)
			}
		}
	}

	return funcs
}

func getCUOWorker(jobs <-chan string, res chan<- OEntry, wg *sync.WaitGroup) {
	defer wg.Done()

	for path := range jobs {
		syms := getFunSymNames(path)
		srcs, ilnFns := getSrcAndInlines(path)

		var cleanedSyms []string
		var removedSyms []string

		for _, s := range syms {
			for _, iFn := range ilnFns {
				if s == iFn {
					removedSyms = append(removedSyms, s)
					break
				}
			}
			cleanedSyms = append(cleanedSyms, s)
		}

		if syms != nil && srcs != nil {
			res <- OEntry{srcs, cleanedSyms, nil, path}
		}
	}
}

func scanKernelObjs(buildPath string) ([]OEntry, error) {
	var entries []OEntry

	jobs := make(chan string, runtime.NumCPU())
	results := make(chan OEntry, runtime.NumCPU())
	var wg sync.WaitGroup

	for range runtime.NumCPU() {
		wg.Add(1)
		go getCUOWorker(jobs, results, &wg)
	}

	go func() {
		filepath.WalkDir(buildPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}

			if d.Type().IsRegular() && filepath.Ext(path) == ".o" && filepath.Base(path) != "vmlinux.o" && !strings.HasSuffix(path, ".mod.o") {
				jobs <- path
			}

			return nil
		})
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	for res := range results {
		entries = append(entries, res)
	}

	return entries, nil
}

func matchObjWorker(jobs <-chan *OEntry, wg *sync.WaitGroup, mtEntries []MaintainerEntry, srcPath string) {
	defer wg.Done()

	for oe := range jobs {
		oe.MtIds = matchMTEntry(mtEntries, oe.Sources, srcPath)
	}
}

func matchKernelObjs(entries []OEntry, mtEntries []MaintainerEntry, srcPath string) {
	jobs := make(chan *OEntry, runtime.NumCPU())
	var wg sync.WaitGroup

	for range runtime.NumCPU() {
		wg.Add(1)
		go matchObjWorker(jobs, &wg, mtEntries, srcPath)
	}

	go func() {
		for i, _ := range entries {
			jobs <- &entries[i]
		}
		close(jobs)
	}()

	wg.Wait()
}

func hash(f *os.File) string {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		log.Fatal(err)
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

func parseMaintainersFile(filePath string) ([]MaintainerEntry, string, error) {
	id := 0

	file, err := os.Open(filePath)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()

	var entries []MaintainerEntry
	var currentEntry MaintainerEntry

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			if currentEntry.Name != "" && (len(currentEntry.regexPatterns) > 0 || len(currentEntry.filePatterns) > 0) {
				entries = append(entries, currentEntry)
				id++
			}
			currentEntry = MaintainerEntry{}
			continue
		}

		currentEntry.id = id

		if currentEntry.Name == "" {
			currentEntry.Name = line
			continue
		}

		if strings.HasPrefix(line, "L:") {
			contact := strings.TrimSpace(line[2:])
			currentEntry.MLContacts = append(currentEntry.MLContacts, contact)
		} else if strings.HasPrefix(line, "M:") || strings.HasPrefix(line, "R:") {
			contact := strings.TrimSpace(line[2:])
			currentEntry.PersonContacts = append(currentEntry.PersonContacts, contact)
		} else if strings.HasPrefix(line, "C:") {
			contact := strings.TrimSpace(line[2:])
			currentEntry.ChatContacts = append(currentEntry.ChatContacts, contact)
		} else if strings.HasPrefix(line, "W:") {
			contact := strings.TrimSpace(line[2:])
			currentEntry.WebContacts = append(currentEntry.WebContacts, contact)
		} else if strings.HasPrefix(line, "B:") {
			contact := strings.TrimSpace(line[2:])
			currentEntry.BugContacts = append(currentEntry.BugContacts, contact)
		} else if strings.HasPrefix(line, "N:") {
			fileMatch := strings.TrimSpace(line[2:])
			currentEntry.regexPatterns = append(currentEntry.regexPatterns, fileMatch)
		} else if strings.HasPrefix(line, "F:") {
			fileMatch := strings.TrimSpace(line[2:])
			currentEntry.filePatterns = append(currentEntry.filePatterns, fileMatch)
		} else if strings.HasPrefix(line, "X:") {
			fileMatch := strings.TrimSpace(line[2:])
			currentEntry.fileAntiPatterns = append(currentEntry.fileAntiPatterns, fileMatch)
		}
	}

	if currentEntry.Name != "" {
		entries = append(entries, currentEntry)
	}

	if err := scanner.Err(); err != nil {
		return nil, "", err
	}

	file.Seek(0, 0)
	return entries, hash(file), nil
}
