package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/user"
	"strconv"
	"strings"

	errors2 "github.com/pkg/errors"
)

const DefaultUploadTempFolder = "filepush"

const DefaultMode = 0764

type FileAPI interface {
	ReadDir(dir string) ([]os.FileInfo, error)
	MakeDirAll(dir string) error
	WriteJSON(file string, content interface{}) error
	Write(file string, content string) error
	ReadJSON(file string, dest interface{}) error
	Exist(path string) (bool, error)
	CreateFile(path string, sourceReader io.Reader) (writtenBytes int64, err error)
	ChangeOwner(path, owner, group string) error
	CreateDirIfNotExists(path string, mode os.FileMode) (wasCreated bool, err error)
	Remove(name string) error
	Rename(oldPath, newPath string) error
}

type FileSystem struct {
}

func NewFileSystem() *FileSystem {
	return &FileSystem{}
}

// ReadDir reads the given directory and returns a list of directory entries sorted by filename.
func (f *FileSystem) ReadDir(dir string) ([]os.FileInfo, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory: %s", err)
	}
	return files, nil
}

// MakeDirAll creates a given directory along with any necessary parents.
// If path is already a directory, it does nothing and returns nil.
// It is created with mode 0777.
func (f *FileSystem) MakeDirAll(dir string) error {
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create dir %q: %s", dir, err)
	}
	return nil
}

// WriteJSON creates or truncates a given file and writes a given content to it as JSON
// with indentation. If the file does not exist, it is created with mode 0666.
func (f *FileSystem) WriteJSON(fileName string, content interface{}) error {
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("failed to create file: %s", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "	")
	if err := encoder.Encode(content); err != nil {
		return fmt.Errorf("failed to write data to file: %v", err)
	}

	return nil
}

// Write creates or truncates a given file and writes a given content to it.
// If the file does not exist, it is created with mode 0666.
func (f *FileSystem) Write(fileName string, content string) error {
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("failed to create file: %s", err)
	}
	defer file.Close()

	if _, err := file.WriteString(content); err != nil {
		return fmt.Errorf("failed to write data to file: %v", err)
	}

	return nil
}

// ReadJSON reads a given file and stores the parsed content into a destination value.
// A successful call returns err == nil, not err == EOF.
func (f *FileSystem) ReadJSON(file string, dest interface{}) error {
	b, err := ioutil.ReadFile(file)
	if err != nil {
		return fmt.Errorf("failed to read data from file: %s", err)
	}

	err = json.Unmarshal(b, dest)
	if err != nil {
		return fmt.Errorf("failed to decode data into %T: %s", dest, err)
	}

	return nil
}

// Exist returns a boolean indicating whether a file or directory with a given path exists.
func (f *FileSystem) Exist(path string) (bool, error) {
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (f *FileSystem) CreateDirIfNotExists(path string, mode os.FileMode) (wasCreated bool, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, errors.New("data directory cannot be empty")
	}

	dirExists, err := f.Exist(path)
	if err != nil {
		return false, errors2.Wrapf(err, "failed to read folder info %s", path)
	}

	if !dirExists {
		err := os.MkdirAll(path, mode)
		if err != nil {
			return false, errors2.Wrapf(err, "failed to create folder %s", path)
		}
		wasCreated = true
	}

	return wasCreated, nil
}

func (f *FileSystem) ChangeOwner(path, owner, group string) error {
	if owner == "" && group == "" {
		return nil
	}

	targetUserUID := os.Getuid()
	if owner != "" {
		usr, err := user.Lookup(owner)
		if err != nil {
			return err
		}
		targetUserUID, err = strconv.Atoi(usr.Uid)
		if err != nil {
			return err
		}
	}

	targetGroupGUID := os.Getgid()
	if group != "" {
		gr, err := user.LookupGroup(group)
		if err != nil {
			return err
		}
		targetGroupGUID, err = strconv.Atoi(gr.Gid)
		if err != nil {
			return err
		}
	}

	err := os.Chown(path, targetUserUID, targetGroupGUID)
	if err != nil {
		return err
	}

	return nil
}

func (f *FileSystem) CreateFile(path string, sourceReader io.Reader) (writtenBytes int64, err error) {
	targetFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, DefaultMode)
	if err != nil {
		return 0, err
	}
	defer targetFile.Close()

	// todo delete file after all operations have completed
	copiedBytes, err := io.Copy(targetFile, sourceReader)
	if err != nil {
		return 0, err
	}

	return copiedBytes, nil
}

func (f *FileSystem) Remove(name string) error {
	return os.Remove(name)
}

func (f *FileSystem) Rename(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}
