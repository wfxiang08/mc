/*
 * Minio Client, (C) 2015, 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package cmd - session V8 - Version 8 stores session header and session data in
// two separate files. Session data contains fully prepared URL list.
package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/minio/mc/pkg/console"
	"github.com/minio/minio/pkg/probe"
	"github.com/minio/minio/pkg/quick"
)

// sessionV8Header for resumable sessions.
type sessionV8Header struct {
	Version            string            `json:"version"`
	When               time.Time         `json:"time"`
	RootPath           string            `json:"workingFolder"`
	GlobalBoolFlags    map[string]bool   `json:"globalBoolFlags"`
	GlobalIntFlags     map[string]int    `json:"globalIntFlags"`
	GlobalStringFlags  map[string]string `json:"globalStringFlags"`
	CommandType        string            `json:"commandType"`
	CommandArgs        []string          `json:"cmdArgs"`
	CommandBoolFlags   map[string]bool   `json:"cmdBoolFlags"`
	CommandIntFlags    map[string]int    `json:"cmdIntFlags"`
	CommandStringFlags map[string]string `json:"cmdStringFlags"`
	LastCopied         string            `json:"lastCopied"`
	LastRemoved        string            `json:"lastRemoved"`
	TotalBytes         int64             `json:"totalBytes"`
	TotalObjects       int64             `json:"totalObjects"`
}

// sessionMessage container for session messages
type sessionMessage struct {
	Status      string    `json:"status"`
	SessionID   string    `json:"sessionId"`
	Time        time.Time `json:"time"`
	CommandType string    `json:"commandType"`
	CommandArgs []string  `json:"commandArgs"`
}

// sessionV8 resumable session container.
type sessionV8 struct {
	Header    *sessionV8Header
	SessionID string
	mutex     *sync.Mutex
	DataFP    *sessionDataFP
	sigCh     bool
}

// sessionDataFP data file pointer.
type sessionDataFP struct {
	dirty bool
	*os.File
}

func (file *sessionDataFP) Write(p []byte) (int, error) {
	file.dirty = true
	return file.File.Write(p)
}

// String colorized session message.
func (s sessionV8) String() string {
	message := console.Colorize("SessionID", fmt.Sprintf("%s -> ", s.SessionID))
	message = message + console.Colorize("SessionTime", fmt.Sprintf("[%s]", s.Header.When.Local().Format(printDate)))
	message = message + console.Colorize("Command", fmt.Sprintf(" %s %s", s.Header.CommandType, strings.Join(s.Header.CommandArgs, " ")))
	return message
}

// JSON jsonified session message.
func (s sessionV8) JSON() string {
	sessionMsg := sessionMessage{
		SessionID:   s.SessionID,
		Time:        s.Header.When.Local(),
		CommandType: s.Header.CommandType,
		CommandArgs: s.Header.CommandArgs,
	}
	sessionMsg.Status = "success"
	sessionBytes, e := json.Marshal(sessionMsg)
	fatalIf(probe.NewError(e), "Unable to marshal into JSON.")

	return string(sessionBytes)
}

// loadSessionV8 - reads session file if exists and re-initiates internal variables
func loadSessionV8(sid string) (*sessionV8, *probe.Error) {
	if !isSessionDirExists() {
		return nil, errInvalidArgument().Trace()
	}
	sessionFile, err := getSessionFile(sid)
	if err != nil {
		return nil, err.Trace(sid)
	}

	if _, e := os.Stat(sessionFile); e != nil {
		return nil, probe.NewError(e)
	}

	// Initialize new session.
	s := &sessionV8{
		Header: &sessionV8Header{
			Version: globalSessionConfigVersion,
		},
		SessionID: sid,
	}

	// Initialize session config loader.
	qs, e := quick.New(s.Header)
	if e != nil {
		return nil, probe.NewError(e).Trace(sid, s.Header.Version)
	}

	if e = qs.Load(sessionFile); e != nil {
		return nil, probe.NewError(e).Trace(sid, s.Header.Version)
	}

	// Validate if the version matches with expected current version.
	sV8Header := qs.Data().(*sessionV8Header)
	if sV8Header.Version != globalSessionConfigVersion {
		msg := fmt.Sprintf("Session header version %s does not match mc session version %s.\n",
			sV8Header.Version, globalSessionConfigVersion)
		return nil, probe.NewError(errors.New(msg)).Trace(sid, sV8Header.Version)
	}

	s.mutex = new(sync.Mutex)
	s.Header = sV8Header

	sessionDataFile, err := getSessionDataFile(s.SessionID)
	if err != nil {
		return nil, err.Trace(sid, s.Header.Version)
	}

	dataFile, e := os.Open(sessionDataFile)
	if e != nil {
		return nil, probe.NewError(e)
	}
	s.DataFP = &sessionDataFP{false, dataFile}

	return s, nil
}

// newSessionV8 provides a new session.
func newSessionV8() *sessionV8 {
	s := &sessionV8{}
	s.Header = &sessionV8Header{}
	s.Header.Version = globalSessionConfigVersion
	// map of command and files copied.
	s.Header.GlobalBoolFlags = make(map[string]bool)
	s.Header.GlobalIntFlags = make(map[string]int)
	s.Header.GlobalStringFlags = make(map[string]string)
	s.Header.CommandArgs = nil
	s.Header.CommandBoolFlags = make(map[string]bool)
	s.Header.CommandIntFlags = make(map[string]int)
	s.Header.CommandStringFlags = make(map[string]string)
	s.Header.When = time.Now().UTC()
	s.mutex = new(sync.Mutex)
	s.SessionID = newRandomID(8)

	sessionDataFile, err := getSessionDataFile(s.SessionID)
	fatalIf(err.Trace(s.SessionID), "Unable to create session data file \""+sessionDataFile+"\".")

	dataFile, e := os.Create(sessionDataFile)
	fatalIf(probe.NewError(e), "Unable to create session data file \""+sessionDataFile+"\".")

	s.DataFP = &sessionDataFP{false, dataFile}

	// Capture state of global flags.
	s.setGlobals()

	return s
}

// HasData provides true if this is a session resume, false otherwise.
func (s sessionV8) HasData() bool {
	return s.Header.LastCopied != "" || s.Header.LastRemoved != ""
}

// NewDataReader provides reader interface to session data file.
func (s *sessionV8) NewDataReader() io.Reader {
	// DataFP is always intitialized, either via new or load functions.
	s.DataFP.Seek(0, os.SEEK_SET)
	return io.Reader(s.DataFP)
}

// NewDataReader provides writer interface to session data file.
func (s *sessionV8) NewDataWriter() io.Writer {
	// DataFP is always intitialized, either via new or load functions.
	s.DataFP.Seek(0, os.SEEK_SET)
	// when moving to file position 0 we want to truncate the file as well,
	// otherwise we'll partly overwrite existing data
	s.DataFP.Truncate(0)
	return io.Writer(s.DataFP)
}

// Save this session.
func (s *sessionV8) Save() *probe.Error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.DataFP.dirty {
		if err := s.DataFP.Sync(); err != nil {
			return probe.NewError(err)
		}
		s.DataFP.dirty = false
	}

	qs, e := quick.New(s.Header)
	if e != nil {
		return probe.NewError(e).Trace(s.SessionID)
	}

	sessionFile, err := getSessionFile(s.SessionID)
	if err != nil {
		return err.Trace(s.SessionID)
	}
	e = qs.Save(sessionFile)
	if e != nil {
		return probe.NewError(e).Trace(sessionFile)
	}
	return nil
}

// setGlobals captures the state of global variables into session header.
// Used by newSession.
func (s *sessionV8) setGlobals() {
	s.Header.GlobalBoolFlags["quiet"] = globalQuiet
	s.Header.GlobalBoolFlags["debug"] = globalDebug
	s.Header.GlobalBoolFlags["json"] = globalJSON
	s.Header.GlobalBoolFlags["noColor"] = globalNoColor
	s.Header.GlobalBoolFlags["insecure"] = globalInsecure
}

// RestoreGlobals restores the state of global variables.
// Used by resumeSession.
func (s sessionV8) restoreGlobals() {
	quiet := s.Header.GlobalBoolFlags["quiet"]
	debug := s.Header.GlobalBoolFlags["debug"]
	json := s.Header.GlobalBoolFlags["json"]
	noColor := s.Header.GlobalBoolFlags["noColor"]
	insecure := s.Header.GlobalBoolFlags["insecure"]
	setGlobals(quiet, debug, json, noColor, insecure)
}

// IsModified - returns if in memory session header has changed from
// its on disk value.
func (s *sessionV8) isModified(sessionFile string) (bool, *probe.Error) {
	qs, e := quick.New(s.Header)
	if e != nil {
		return false, probe.NewError(e).Trace(s.SessionID)
	}

	var currentHeader = &sessionV8Header{}
	currentQS, e := quick.Load(sessionFile, currentHeader)
	if e != nil {
		// If session does not exist for the first, return modified to
		// be true.
		if os.IsNotExist(e) {
			return true, nil
		}
		// For all other errors return.
		return false, probe.NewError(e).Trace(s.SessionID)
	}

	changedFields, e := qs.DeepDiff(currentQS)
	if e != nil {
		return false, probe.NewError(e).Trace(s.SessionID)
	}

	// Returns true if there are changed entries.
	return len(changedFields) > 0, nil
}

// save - wrapper for quick.Save and saves only if sessionHeader is
// modified.
func (s *sessionV8) save() *probe.Error {
	sessionFile, err := getSessionFile(s.SessionID)
	if err != nil {
		return err.Trace(s.SessionID)
	}

	// Verify if sessionFile is modified.
	modified, err := s.isModified(sessionFile)
	if err != nil {
		return err.Trace(s.SessionID)
	}
	// Header is modified, we save it.
	if modified {
		qs, e := quick.New(s.Header)
		if e != nil {
			return probe.NewError(e).Trace(s.SessionID)
		}
		// Save an return.
		e = qs.Save(sessionFile)
		if e != nil {
			return probe.NewError(e).Trace(sessionFile)
		}
	}
	return nil
}

// Close ends this session and removes all associated session files.
func (s *sessionV8) Close() *probe.Error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if err := s.DataFP.Close(); err != nil {
		return probe.NewError(err)
	}

	// Attempt to save the header if modified.
	return s.save()
}

// Delete removes all the session files.
func (s *sessionV8) Delete() *probe.Error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.DataFP != nil {
		name := s.DataFP.Name()
		// close file pro-actively before deleting
		// ignore any error, it could be possibly that
		// the file is closed already
		s.DataFP.Close()

		// Remove the data file.
		if e := os.Remove(name); e != nil {
			return probe.NewError(e)
		}
	}

	// Fetch the session file.
	sessionFile, err := getSessionFile(s.SessionID)
	if err != nil {
		return err.Trace(s.SessionID)
	}

	// Remove session file
	if e := os.Remove(sessionFile); e != nil {
		return probe.NewError(e)
	}

	// Remove session backup file if any, ignore any error.
	os.Remove(sessionFile + ".old")

	return nil
}

// Close a session and exit.
func (s sessionV8) CloseAndDie() {
	s.Close()
	console.Fatalln("Session safely terminated. To resume session ‘mc session resume " + s.SessionID + "’")
}

// Create a factory function to simplify checking if
// object was last operated on.
func isLastFactory(lastURL string) func(string) bool {
	last := true // closure
	return func(sourceURL string) bool {
		if sourceURL == "" {
			fatalIf(errInvalidArgument().Trace(), "Empty source argument passed.")
		}
		if lastURL == "" {
			return false
		}

		if last {
			if lastURL == sourceURL {
				last = false // from next call onwards we say false.
			}
			return true
		}
		return false
	}
}
