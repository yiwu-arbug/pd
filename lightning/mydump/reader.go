package mydump

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/pingcap/tidb-lightning/lightning/common"
	"github.com/pkg/errors"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
)

var (
	insStmtRegex = regexp.MustCompile(`(?i)INSERT INTO .* VALUES`)
)

var (
	ErrInsertStatementNotFound = errors.New("insert statement not found")
	errInvalidSchemaEncoding   = errors.New("invalid schema encoding")
)

var (
	supportedSchemaEncodings = []encoding.Encoding{
		simplifiedchinese.GB18030,
	}
)

func decodeCharacterSet(data []byte, characterSet string) ([]byte, error) {
	switch characterSet {
	case "binary":
		// do nothing
	case "auto", "utf8mb4":
		if utf8.Valid(data) {
			break
		}
		if characterSet == "utf8mb4" {
			return nil, errInvalidSchemaEncoding
		}
		// try gb18030 next if the encoding is "auto"
		// if we support too many encodings, consider switching strategy to
		// perform `chardet` first.
		fallthrough
	case "gb18030":
		decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data)
		if err != nil {
			return nil, errors.Trace(err)
		}
		// check for U+FFFD to see if decoding contains errors.
		// https://groups.google.com/d/msg/golang-nuts/pENT3i4zJYk/v2X3yyiICwAJ
		if bytes.ContainsRune(decoded, '\ufffd') {
			return nil, errInvalidSchemaEncoding
		}
		data = decoded
	default:
		return nil, errors.Errorf("Unsupported encoding %s", characterSet)
	}
	return data, nil
}

func ExportStatement(sqlFile string, characterSet string) ([]byte, error) {
	fd, err := os.Open(sqlFile)
	if err != nil {
		return nil, errors.Trace(err)
	}
	defer fd.Close()

	br := bufio.NewReader(fd)
	f, err := os.Stat(sqlFile)
	if err != nil {
		return nil, errors.Trace(err)
	}

	data := make([]byte, 0, f.Size()+1)
	buffer := make([]byte, 0, f.Size()+1)
	for {
		line, err := br.ReadString('\n')
		if errors.Cause(err) == io.EOF { // it will return EOF if there is no trailing new line.
			if len(line) == 0 {
				break
			}
		} else {
			line = strings.TrimSpace(line[:len(line)-1])
		}

		if len(line) == 0 {
			continue
		}

		buffer = append(buffer, []byte(line)...)
		if buffer[len(buffer)-1] == ';' {
			statement := string(buffer)
			if !(strings.HasPrefix(statement, "/*") && strings.HasSuffix(statement, "*/;")) {
				data = append(data, buffer...)
			}
			buffer = buffer[:0]
		} else {
			buffer = append(buffer, '\n')
		}
	}

	data, err = decodeCharacterSet(data, characterSet)
	if err != nil {
		common.AppLogger.Errorf("cannot decode input file as %s encoding, please convert it manually: %s", characterSet, sqlFile)
		return nil, errors.Annotatef(err, "failed to decode %s as %s", sqlFile, characterSet)
	}
	return data, nil
}

type MDDataReader struct {
	fd         *os.File
	file       string
	fsize      int64
	start      int64
	stmtHeader []byte

	bufferSize int64
	buffer     *bufio.Reader
	// readBuffer []byte
}

func NewMDDataReader(file string, offset int64) (*MDDataReader, error) {
	fd, err := os.Open(file)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if _, err := fd.Seek(offset, io.SeekStart); err != nil {
		fd.Close()
		return nil, errors.Trace(err)
	}

	fstat, err := fd.Stat()
	if err != nil {
		fd.Close()
		return nil, errors.Trace(err)
	}

	mdr := &MDDataReader{
		fd:         fd,
		fsize:      fstat.Size(),
		file:       file,
		start:      offset,
		stmtHeader: getInsertStatementHeader(file),
	}

	if len(mdr.stmtHeader) == 0 {
		return nil, ErrInsertStatementNotFound
	}

	mdr.skipAnnotation(offset)
	return mdr, nil
}

func (r *MDDataReader) Close() error {
	if r.fd != nil {
		if err := r.fd.Close(); err != nil {
			return errors.Trace(err)
		}
	}

	r.buffer = nil
	r.bufferSize = 0
	return nil
}

func (r *MDDataReader) skipAnnotation(offset int64) int64 {
	br := bufio.NewReader(r.fd)
	for skipSize := 0; ; {
		line, err := br.ReadString('\n')
		if errors.Cause(err) == io.EOF {
			break
		}

		size := len(line)
		line = strings.TrimSpace(line[:size-1])
		if !(strings.HasPrefix(line, "/*") && strings.HasSuffix(line, "*/;")) {
			// backward seek to the last pos
			// note! seeking beyond EOF won't trigger any error,
			// and *will* cause Tell() return the wrong value. https://stackoverflow.com/q/17263830/
			offset += int64(skipSize)
			if offset > r.fsize {
				offset = r.fsize
			}
			r.fd.Seek(offset, io.SeekStart)
			break
		}
		skipSize += size
	}

	return r.currOffset()
}

func (r *MDDataReader) Seek(offset int64) int64 {
	return r.skipAnnotation(offset)
}

func (r *MDDataReader) Tell() int64 {
	return r.currOffset()
}

func (r *MDDataReader) currOffset() int64 {
	off, err := r.fd.Seek(0, io.SeekCurrent)
	if err != nil {
		common.AppLogger.Errorf("get file offset failed (%s) : %v", r.file, err)
		return -1
	}
	return off
}

func getInsertStatementHeader(file string) []byte {
	f, err := os.Open(file)
	if err != nil {
		common.AppLogger.Errorf("open file failed (%s) : %v", file, err)
		return []byte{}
	}
	defer f.Close()

	header := ""
	br := bufio.NewReaderSize(f, int(defReadBlockSize))
	for {
		line, err := br.ReadString('\n')
		if errors.Cause(err) == io.EOF {
			break
		}
		if loc := insStmtRegex.FindStringIndex(line); len(loc) > 0 {
			header = line[loc[0]:loc[1]]
			break
		}
	}

	return []byte(header)
}

func (r *MDDataReader) acquireBufferReader(fd *os.File, size int64) *bufio.Reader {
	if size > r.bufferSize {
		r.buffer = bufio.NewReaderSize(fd, int(size))
		r.bufferSize = size
	}
	return r.buffer
}

func (r *MDDataReader) Read(minSize int64) ([][]byte, error) {
	fd, beginPos := r.fd, r.currOffset()
	if beginPos >= r.fsize {
		return nil, io.EOF
	}

	reader := r.acquireBufferReader(fd, minSize<<1)
	defer reader.Reset(fd)

	// split file's content into multi sql statement
	var stmts = make([][]byte, 0, 8)
	appendSQL := func(sql []byte) {
		sql = bytes.TrimSpace(sql)
		sqlLen := len(sql)
		if sqlLen != 0 {
			// TODO : check  "/* xxx */;"

			// check prefix
			if !bytes.HasPrefix(sql, r.stmtHeader) {
				common.AppLogger.Errorf("Unexpect sql starting : '%s ..'", string(sql)[:10])
				return
			}
			if sqlLen == len(r.stmtHeader) {
				return // ps : empty sql statement without any actual values ~
			}

			// check suffix
			if !bytes.HasSuffix(sql, []byte(";")) {
				if bytes.HasSuffix(sql, []byte(",")) {
					sql[sqlLen-1] = ';'
				} else {
					common.AppLogger.Errorf("Unexpect sql ending : '.. %s'", string(sql)[sqlLen-10:])
					return
				}
			}

			stmts = append(stmts, sql)
		}
	}

	/*
		Read file in specified format like :
		'''
			INSERT INTO xxx VALUES
			(...),
			(...),
			(...);
		'''
	*/
	var statement = make([]byte, 0, minSize+4096)
	var readSize, lineSize int64
	var line []byte
	var err error

	/*
		TODO :
			1. "(...);INSERT INTO .."
			2. huge line
	*/
	for end := false; !end; {
		line, err = reader.ReadBytes('\n')
		lineSize = int64(len(line))
		end = (err == io.EOF)

		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			if line[0] == '/' &&
				bytes.HasPrefix(line, []byte("/*")) && bytes.HasSuffix(line, []byte("*/")) {
				// ps : is a comment, ignored it
				// TODO : what if comment with span on multi lines ?
				continue
			}

			if len(statement) == 0 && !bytes.HasPrefix(line, r.stmtHeader) {
				statement = append(statement, r.stmtHeader...)
			}
			statement = append(statement, line...)

			if statement[len(statement)-1] == ';' {
				appendSQL(statement)
				statement = make([]byte, 0, minSize+4096)
			}
		}

		readSize += lineSize
		if readSize >= minSize {
			fd.Seek(beginPos+readSize, io.SeekStart) // ps : as buffer reader over readed !
			break
		}
	}

	if len(statement) > 0 {
		appendSQL(statement)
	}

	return stmts, nil
}

/////////////////////////////////////////////////////////////////////////

type RegionReader struct {
	fileReader *MDDataReader
	offset     int64
	size       int64
	pos        int64
}

func NewRegionReader(file string, offset int64, size int64) (*RegionReader, error) {
	common.AppLogger.Debugf("[%s] offset = %d / size = %d", file, offset, size)

	fileReader, err := NewMDDataReader(file, offset)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &RegionReader{
		fileReader: fileReader,
		size:       size,
		offset:     offset,
		pos:        fileReader.Tell(),
	}, nil
}

func (r *RegionReader) Read(maxBlockSize int64) ([][]byte, error) {
	if r.pos >= r.offset+r.size {
		return [][]byte{}, io.EOF
	}

	readSize := r.offset + r.size - r.pos
	if maxBlockSize < readSize {
		readSize = maxBlockSize
	}

	datas, err := r.fileReader.Read(readSize)
	r.pos = r.fileReader.Tell()

	return datas, errors.Trace(err)
}

func (r *RegionReader) Tell() int64 {
	return r.pos
}

func (r *RegionReader) Seek(pos int64) {
	r.pos = r.fileReader.Seek(pos)
}

func (r *RegionReader) Size() int64 {
	return r.size
}

func (r *RegionReader) Close() error {
	return errors.Trace(r.fileReader.Close())
}