package syslog

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jeromer/syslogparser"
	"github.com/jeromer/syslogparser/rfc3164"
	"github.com/jeromer/syslogparser/rfc5424"

	"github.com/qiniu/logkit/conf"
	"github.com/qiniu/logkit/parser"
	. "github.com/qiniu/logkit/parser/config"
	. "github.com/qiniu/logkit/utils/models"
)

const (
	detectedRFC3164 = iota
	detectedRFC5424 = iota
	detectedRFC6587 = iota
	detectedLeftLog = iota
)

func init() {
	parser.RegisterConstructor(TypeSyslog, NewParser)
}

type LogParts map[string]interface{}

type Parser interface {
	Parse() error
	Dump() LogParts
	Location(*time.Location)
}

type Format interface {
	GetParser([]byte) Parser
	IsNewLine(data []byte) bool
}

type parserWrapper struct {
	syslogparser.LogParser
}

func (w *parserWrapper) Dump() LogParts {
	return LogParts(w.LogParser.Dump())
}

func DetectType(data []byte) (detected int) {
	// all formats have a sapce somewhere
	if i := bytes.IndexByte(data, ' '); i > 0 {
		pLength := data[0:i]
		if _, err := strconv.Atoi(string(pLength)); err == nil {
			return detectedRFC6587
		}
		if len(data) < 1 || data[0] != '<' {
			return detectedLeftLog
		}
		// 开头由一对尖括号组成 <12>
		angle := bytes.IndexByte(data, '>')
		if (angle < 0) || (angle >= i) {
			return detectedLeftLog
		}

		//中间是0-9
		for j := 1; j < angle; j++ {
			if data[j] < '0' || data[j] > '9' {
				return detectedLeftLog
			}
		}

		// <1>1 尖括号后紧跟数字的是RFC5424
		// 否则是 RFC3164
		if (angle+2 == i) && (data[angle+1] >= '0') && (data[angle+1] <= '9') {
			return detectedRFC5424
		} else {
			return detectedRFC3164
		}
	}
	return detectedLeftLog
}

func GetFormt(format string) Format {
	switch strings.ToLower(format) {
	case "rfc3164":
		return &RFC3164{}
	case "rfc5424":
		return &RFC5424{}
	case "rfc6587":
		return &RFC6587{}
	}
	return &Automatic{}
}

func NewParser(c conf.MapConf) (parser.Parser, error) {
	name, _ := c.GetStringOr(KeyParserName, "")
	labelList, _ := c.GetStringListOr(KeyLabels, []string{})
	rfctype, _ := c.GetStringOr(KeyRFCType, "automic")
	maxline, _ := c.GetIntOr(KeySyslogMaxline, 100)

	nameMap := make(map[string]struct{})
	labels := GetGrokLabels(labelList, nameMap)

	disableRecordErrData, _ := c.GetBoolOr(KeyDisableRecordErrData, false)
	keepRawData, _ := c.GetBoolOr(KeyKeepRawData, false)

	format := GetFormt(rfctype)
	buff := bytes.NewBuffer([]byte{})
	numRoutine := MaxProcs
	if numRoutine == 0 {
		numRoutine = 1
	}
	return &SyslogParser{
		name:                 name,
		labels:               labels,
		buff:                 buff,
		format:               format,
		disableRecordErrData: disableRecordErrData,
		maxline:              maxline,
		curline:              0,
		keepRawData:          keepRawData,
		numRoutine:           numRoutine,
	}, nil
}

type SyslogParser struct {
	name                 string
	labels               []GrokLabel
	buff                 *bytes.Buffer
	format               Format
	maxline              int
	curline              int
	disableRecordErrData bool
	keepRawData          bool

	numRoutine int
}

func (p *SyslogParser) Name() string {
	return p.name
}

func (p *SyslogParser) Type() string {
	return TypeSyslog
}

func (p *SyslogParser) Parse(lines []string) ([]Data, error) {
	var (
		lineLen = len(lines)
		datas   = make([]Data, lineLen)
		se      = &StatsError{}

		numRoutine = p.numRoutine
		sendChan   = make(chan parser.ParseInfo)
		resultChan = make(chan parser.ParseResult)
		wg         = new(sync.WaitGroup)
	)

	if lineLen < numRoutine {
		numRoutine = lineLen
	}

	if p.buff.Len() == 0 && len(lines) == 1 && lines[0] == PandoraParseFlushSignal {
		return []Data{}, nil
	}

	for i := 0; i < numRoutine; i++ {
		wg.Add(1)
		go parser.ParseLine(sendChan, resultChan, wg, true, p.parse)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	go func() {
		for idx, line := range lines {
			sendChan <- parser.ParseInfo{
				Line:  line,
				Index: idx,
			}
		}
		close(sendChan)
	}()

	var parseResultSlice = make(parser.ParseResultSlice, lineLen)
	for resultInfo := range resultChan {
		parseResultSlice[resultInfo.Index] = resultInfo
	}

	se.DatasourceSkipIndex = make([]int, lineLen)
	datasourceIndex := 0
	dataIndex := 0
	for _, parseResult := range parseResultSlice {
		if len(parseResult.Line) == 0 {
			se.DatasourceSkipIndex[datasourceIndex] = parseResult.Index
			datasourceIndex++
			continue
		}

		if parseResult.Err != nil {
			se.AddErrors()
			se.LastError = parseResult.Err.Error()
			if p.disableRecordErrData && !p.keepRawData {
				se.DatasourceSkipIndex[datasourceIndex] = parseResult.Index
				datasourceIndex++
			}
			if parseResult.Data != nil {
				datas[dataIndex] = parseResult.Data
				dataIndex++
			}
			continue
		}
		if len(parseResult.Data) < 1 { //数据为空时不发送
			se.AddSuccess()
			continue
		}
		for _, label := range p.labels {
			parseResult.Data[label.Name] = label.Value
		}
		se.AddSuccess()
		if p.keepRawData {
			parseResult.Data[KeyRawData] = parseResult.Line
		}
		datas[dataIndex] = parseResult.Data
		dataIndex++
	}

	se.DatasourceSkipIndex = se.DatasourceSkipIndex[:datasourceIndex]
	datas = datas[:dataIndex]
	if se.Errors == 0 {
		return datas, nil
	}
	return datas, se
}

func (p *SyslogParser) parse(line string) (data Data, err error) {
	if p.buff.Len() > 0 {
		if line == PandoraParseFlushSignal {
			return p.Flush()
		}

		if p.curline >= p.maxline || p.format.IsNewLine([]byte(line)) {
			data, err = p.Flush()
			if err != nil {
				return data, err
			}
		} else {
			p.curline++
		}
	}

	if line != PandoraParseFlushSignal {
		_, err = p.buff.Write([]byte(line))
		if err != nil {
			if !p.disableRecordErrData || p.keepRawData {
				data = make(Data)
			}
			if !p.disableRecordErrData {
				data[KeyPandoraStash] = string(p.buff.Bytes())
			}
			if p.keepRawData {
				data[KeyRawData] = string(p.buff.Bytes())
			}
		}
		return data, err
	}
	return data, nil
}

func (p *SyslogParser) Flush() (data Data, err error) {
	sparser := p.format.GetParser(p.buff.Bytes())
	err = sparser.Parse()
	if err == nil || err.Error() == "No structured data" {
		data = Data(sparser.Dump())
		err = nil
	} else {
		if p.curline == p.maxline {
			err = fmt.Errorf("syslog meet max line %v, try to parse err %v, check if this is standard rfc3164/rfc5424 syslog", p.maxline, err)
		}
		if !p.disableRecordErrData || p.keepRawData {
			data = make(Data)
		}
		if !p.disableRecordErrData {
			data[KeyPandoraStash] = string(p.buff.Bytes())
		}
		if p.keepRawData {
			data[KeyRawData] = string(p.buff.Bytes())
		}
	}
	p.curline = 0
	p.buff.Reset()
	return data, err
}

type RFC6587 struct{}

func (f *RFC6587) GetParser(line []byte) Parser {
	return &parserWrapper{rfc5424.NewParser(line)}
}

func (f *RFC6587) IsNewLine(data []byte) bool {
	if i := bytes.IndexByte(data, ' '); i > 0 {
		pLength := data[0:i]
		_, err := strconv.Atoi(string(pLength))
		if err != nil {
			if string(data[0:1]) == "<" {
				// Assume this frame uses non-transparent-framing
				return true
			}
			return false
		}
		return true
	}
	return false
}

type RFC5424 struct{}

func (f *RFC5424) GetParser(line []byte) Parser {
	return &parserWrapper{rfc5424.NewParser(line)}
}

func (f *RFC5424) IsNewLine(data []byte) bool {
	// all formats have a sapce somewhere
	if i := bytes.IndexByte(data, ' '); i > 0 {
		if len(data) < 1 || data[0] != '<' {
			return false
		}
		// 开头由一对尖括号组成 <12>
		angle := bytes.IndexByte(data, '>')
		if (angle < 0) || (angle >= i) {
			return false
		}
		// <1>1 尖括号后紧跟数字的是RFC5424
		// 否则是 RFC3164
		if (angle+2 == i) && (data[angle+1] >= '0') && (data[angle+1] <= '9') {
			return true
		}
		return false
	}
	return false
}

type RFC3164 struct{}

func (f *RFC3164) GetParser(line []byte) Parser {
	return &parserWrapper{rfc3164.NewParser(line)}
}

func (f *RFC3164) IsNewLine(data []byte) bool {
	if i := bytes.IndexByte(data, ' '); i > 1 {
		if string(data[0:1]) != "<" || string(data[i-1:i]) != ">" {
			return false
		}
		return true
	}
	return false
}

type Automatic struct{}

func (f *Automatic) GetParser(line []byte) Parser {
	switch format := DetectType(line); format {
	case detectedRFC3164:
		return &parserWrapper{rfc3164.NewParser(line)}
	case detectedRFC5424:
		return &parserWrapper{rfc5424.NewParser(line)}
	default:
		return &parserWrapper{rfc3164.NewParser(line)}
	}
}

func (f *Automatic) IsNewLine(data []byte) bool {
	switch format := DetectType(data); format {
	case detectedRFC6587, detectedRFC3164, detectedRFC5424:
		return true
	}
	return false
}
