package mysql_replication_listener

import (
	"fmt"
	"io"
)

type (
	eventLog struct {
		mysqlConnection *connection
		binlogVersion   uint16

		lastRotatePosition uint32
		lastRotateFileName []byte

		headerQueryEventLength        byte
		headerDeleteRowsEventV1Length byte
		headerUpdateRowsEventV1Length byte
		headerWriteRowsEventV1Length  byte
	}

	eventLogHeader struct {
		Timestamp    uint32
		EventType    byte
		ServerId     uint32
		EventSize    uint32
		NextPosition uint32
		Flags        uint16
	}

	logRotateEvent struct {
		*eventLogHeader
		position       uint64
		binlogFileName []byte
	}

	formatDescriptionEvent struct {
		*eventLogHeader
		binlogVersion          uint16
		mysqlServerVersion     []byte
		createTimestamp        uint32
		eventTypeHeaderLengths []byte
	}

	startEventV3Event struct {
		*eventLogHeader
		binlogVersion      uint16
		mysqlServerVersion []byte
		createTimestamp    uint32
	}

	QueryEvent struct {
		*eventLogHeader
		SlaveProxyId  uint32
		ExecutionTime uint32
		ErrorCode     uint16
		StatusVars    []byte
		Schema        string
		Query         string
		binLogVersion uint16
	}

	XidEvent struct {
		*eventLogHeader
		TransactionId uint64
	}

	IntVarEvent struct {
		*eventLogHeader
		Type  byte
		Value uint64
	}

	BeginLoadQueryEvent struct {
		*eventLogHeader
		FileId    uint32
		BlockData string
	}

	ExecuteLoadQueryEvent struct {
		*eventLogHeader
		SlaveProxyId     uint32
		ExecutionTime    uint32
		ErrorCode        uint16
		StatusVars       []byte
		Schema           string
		FileId           uint32
		StartPos         uint32
		EndPos           uint32
		DupHandlingFlags byte
		Query            string
	}

	UserVarEvent struct {
		*eventLogHeader
		Name    string
		IsNil   bool
		Type    byte
		Charset uint32
		Value   string
		Flags   byte
	}

	IncidentEvent struct {
		*eventLogHeader
		Type    uint16
		Message string
	}

	RandEvent struct {
		*eventLogHeader
		Seed1 uint64
		Seed2 uint64
	}

	TableMapEvent struct {
		*eventLogHeader
		TableId          uint64
		Flags            uint16
		SchemaName       string
		TableName        string
		Columns          []*TableMapEventColumn
		columnTypeDef    []byte
		columnMetaDef    []byte
		columnNullBitMap []byte
	}

	TableMapEventColumn struct {
		Type     byte
		MetaInfo []byte
		Null     bool
	}

	UpdateRowsEvent struct {
		postHeaderLength byte
		version          byte
		*eventLogHeader
		TableId              uint64
		ExtraData            []byte
		ColumnPresentBitmap1 []byte
		ColumnPresentBitmap2 []byte
	}

	unknownEvent struct {
		*eventLogHeader
	}

	binLogEvent interface {
		read(*pack)
	}

	AppendBlockEvent BeginLoadQueryEvent
	ignorableEvent   unknownEvent
	HeartBeatEvent   unknownEvent
	StopEvent        unknownEvent
	slaveEvent       unknownEvent
)

func (event *UpdateRowsEvent) setPostHeaderLength(length byte) {
	event.postHeaderLength = length
}

func (event *UpdateRowsEvent) setVersion(version byte) {
	event.version = version
}

func (event *UpdateRowsEvent) read(pack *pack) {
	if event.postHeaderLength == 6 {
		var tableId uint32
		pack.readUint32(&tableId)
		event.TableId = uint64(tableId)
	} else {
		pack.readSixByteUint64(&event.TableId)
	}

	if event.version == 2 {
		var extraDataLength uint16
		pack.readUint16(&extraDataLength)
		event.ExtraData = pack.Next(int(extraDataLength))
	}

	var (
		columnCount uint64
		isNull      bool
	)

	pack.readIntLengthOrNil(&columnCount, &isNull)

	bitMapLength := int((columnCount + 7) / 8)

	event.ColumnPresentBitmap1 = pack.Next(bitMapLength)
	if event.version >= 1 {
		event.ColumnPresentBitmap2 = pack.Next(bitMapLength)
	}

}

func (event *TableMapEvent) read(pack *pack) {
	pack.readSixByteUint64(&event.TableId)
	pack.readUint16(&event.Flags)

	schemaLength, _ := pack.ReadByte()
	event.SchemaName = string(pack.Next(int(schemaLength)))
	filler, _ := pack.ReadByte()
	if filler != 0 {
		panic("incorrect filler")
	}

	tableLength, _ := pack.ReadByte()
	event.TableName = string(pack.Next(int(tableLength)))
	filler, _ = pack.ReadByte()
	if filler != 0 {
		panic("incorrect filler")
	}

	var columnCount uint64
	var isNull bool

	pack.readIntLengthOrNil(&columnCount, &isNull)

	event.columnTypeDef = pack.Next(int(columnCount))
	event.columnMetaDef, _ = pack.readStringLength()
	event.columnNullBitMap = pack.Bytes()
	//	fmt.Printf("% x\n", event.columnNullBitMap)

	event.Columns = make([]*TableMapEventColumn, columnCount)

	metaOffset := 0

	for i := 0; i < len(event.columnTypeDef); i++ {
		column := &TableMapEventColumn{
			Type: event.columnTypeDef[i],
			Null: (event.columnNullBitMap[i/8] >> uint8(i%8)) & 1 == 1,
		}

		switch event.columnTypeDef[i]{
		case _MYSQL_TYPE_STRING, _MYSQL_TYPE_VAR_STRING, _MYSQL_TYPE_VARCHAR, _MYSQL_TYPE_DECIMAL,
		_MYSQL_TYPE_NEWDECIMAL, _MYSQL_TYPE_ENUM, _MYSQL_TYPE_SET:
			column.MetaInfo = event.columnMetaDef[metaOffset:metaOffset+2]
			metaOffset+=2
		case _MYSQL_TYPE_BLOB, _MYSQL_TYPE_DOUBLE, _MYSQL_TYPE_FLOAT:
			column.MetaInfo = event.columnMetaDef[metaOffset:metaOffset+1]
			metaOffset+=1
		default:
			column.MetaInfo = []byte{}
		}

		event.Columns[i] = column
	}
}

func (event *RandEvent) read(pack *pack) {
	pack.readUint64(&event.Seed1)
	pack.readUint64(&event.Seed2)
}

func (event *IncidentEvent) read(pack *pack) {
	pack.readUint16(&event.Type)
	length, _ := pack.ReadByte()
	event.Message = string(pack.Next(int(length)))
}

func (event *unknownEvent) read(pack *pack) {

}

func (event *UserVarEvent) read(pack *pack) {
	var nameLength uint32
	pack.readUint32(&nameLength)
	event.Name = string(pack.Next(int(nameLength)))
	isNull, _ := pack.ReadByte()
	event.IsNil = isNull == 1
	if event.IsNil {
		return
	}

	event.Type, _ = pack.ReadByte()
	pack.readUint32(&event.Charset)
	var length uint32
	pack.readUint32(&length)
	event.Value = string(pack.Next(int(length)))
	event.Flags, _ = pack.ReadByte()
}

func (event *ExecuteLoadQueryEvent) read(pack *pack) {
	pack.readUint32(&event.SlaveProxyId)
	pack.readUint32(&event.ExecutionTime)

	schemaLength, _ := pack.ReadByte()

	pack.readUint16(&event.ErrorCode)

	var statusVarsLength uint16
	pack.readUint16(&statusVarsLength)

	pack.readUint32(&event.FileId)
	pack.readUint32(&event.StartPos)
	pack.readUint32(&event.EndPos)
	event.DupHandlingFlags, _ = pack.ReadByte()

	event.StatusVars = pack.Next(int(statusVarsLength))
	event.Schema = string(pack.Next(int(schemaLength)))

	splitter, _ := pack.ReadByte()

	if splitter != 0 {
		panic("Incorrect binlog EXECUTE_LOAD_QUERY_EVENT structure")
	}

	event.Query = string(pack.Bytes())
}

func (event *BeginLoadQueryEvent) read(pack *pack) {
	pack.readUint32(&event.FileId)
	event.BlockData = string(pack.Bytes())
}

func (event *IntVarEvent) read(pack *pack) {
	event.Type, _ = pack.ReadByte()
	pack.readUint64(&event.Value)
}

func (event *XidEvent) read(pack *pack) {
	pack.readUint64(&event.TransactionId)
}

func (event *QueryEvent) read(pack *pack) {
	pack.readUint32(&event.SlaveProxyId)
	pack.readUint32(&event.ExecutionTime)

	schemaLength, _ := pack.ReadByte()

	pack.readUint16(&event.ErrorCode)

	if event.binLogVersion >= 4 {
		var statusVarsLength uint16
		pack.readUint16(&statusVarsLength)
		event.StatusVars = pack.Next(int(statusVarsLength))
	}

	event.Schema = string(pack.Next(int(schemaLength)))

	splitter, _ := pack.ReadByte()

	if splitter != 0 {
		panic("Incorrect binlog QUERY_EVENT structure")
	}

	event.Query = string(pack.Bytes())
}

func (event *logRotateEvent) read(pack *pack) {
	pack.readUint64(&event.position)
	event.binlogFileName = pack.Bytes()
}

func (event *formatDescriptionEvent) read(pack *pack) {
	pack.readUint16(&event.binlogVersion)
	event.mysqlServerVersion = pack.Next(50)
	pack.readUint32(&event.createTimestamp)
	length, _ := pack.ReadByte()
	event.eventTypeHeaderLengths = pack.Next(int(length))
}

func (event *startEventV3Event) read(pack *pack) {
	pack.readUint16(&event.binlogVersion)
	event.mysqlServerVersion = make([]byte, 50)
	pack.Read(event.mysqlServerVersion)

	pack.readUint32(&event.createTimestamp)
}

func (eh *eventLogHeader) read(pack *pack) {
	pack.ReadByte()
	pack.readUint32(&eh.Timestamp)
	eh.EventType, _ = pack.ReadByte()
	pack.readUint32(&eh.ServerId)
	pack.readUint32(&eh.EventSize)
	pack.readUint32(&eh.NextPosition)
	pack.readUint16(&eh.Flags)
}

func newEventLog(mysqlConnection *connection) *eventLog {
	return &eventLog{
		mysqlConnection: mysqlConnection,
	}
}

func (ev *eventLog) start() {
	for {
		event, err := ev.readEvent()
		if err != nil {
			if err == io.EOF {
				println("EOF")
				break
			}
		}

		switch e := event.(type) {
		case *startEventV3Event:
			ev.binlogVersion = e.binlogVersion
		case *formatDescriptionEvent:
			ev.binlogVersion = e.binlogVersion
			ev.headerQueryEventLength = e.eventTypeHeaderLengths[_FORMAT_DESCRIPTION_LENGTH_QUERY_POSITION]

			ev.headerDeleteRowsEventV1Length = 6
			ev.headerUpdateRowsEventV1Length = 6
			ev.headerWriteRowsEventV1Length = 6

			if len(e.eventTypeHeaderLengths) >= 24 {
				ev.headerDeleteRowsEventV1Length = e.eventTypeHeaderLengths[_FORMAT_DESCRIPTION_LENGTH_DELETEV1_POSITION]
				ev.headerUpdateRowsEventV1Length = e.eventTypeHeaderLengths[_FORMAT_DESCRIPTION_LENGTH_UPDATEV1_POSITION]
				ev.headerWriteRowsEventV1Length = e.eventTypeHeaderLengths[_FORMAT_DESCRIPTION_LENGTH_WRITEV1_POSITION]
			}

		case *logRotateEvent:
			ev.lastRotateFileName = e.binlogFileName
			println("rotate", e.position, string(e.binlogFileName))
		case *QueryEvent:
			println(e.Query)
			//redirect to chan
		case *XidEvent:
			println(e.TransactionId)
			//redirect to chan
		case *IntVarEvent:
			println(e.Type)
			println(e.Value)
			//redirect to chan
		case *BeginLoadQueryEvent:
			println(e.BlockData)
			//redirect to chan
		case *AppendBlockEvent:
			println(e.BlockData)
			//redirect to chan
		case *ExecuteLoadQueryEvent:
			println(e.Query)
			//redirect to chan
		case *UserVarEvent:
			println(e.Name, "=", e.Value)
			//redirect to chan
		case *StopEvent:
			//redirect to chan
			println("stop")
		case *IncidentEvent:
			//redirect to chan
			println("incident")
		case *RandEvent:
			//redirect to chan
			println("rand")
		case *TableMapEvent:
			//redirect to chan
			println("table event")
			////////// trash events
		case *slaveEvent:
			//no action
		case *unknownEvent:
			//no action
		case *ignorableEvent:
			//no action
		case *HeartBeatEvent:
			//no action
		}
	}
}

func (ev *eventLog) readEvent() (interface{}, error) {
	pack, err := ev.mysqlConnection.packReader.readNextPack()

	if err != nil {
		return nil, err
	}

	header := &eventLogHeader{}
	header.read(pack)

	var event binLogEvent

	switch header.EventType {
	case _START_EVENT_V3:
		event = &startEventV3Event{
			eventLogHeader: header,
		}
	case _FORMAT_DESCRIPTION_EVENT:
		event = &formatDescriptionEvent{
			eventLogHeader: header,
		}
	case _ROTATE_EVENT:
		event = &logRotateEvent{
			eventLogHeader: header,
		}
	case _QUERY_EVENT:
		event = &QueryEvent{
			eventLogHeader: header,
			binLogVersion:  ev.binlogVersion,
		}
	case _XID_EVENT:
		event = &XidEvent{
			eventLogHeader: header,
		}
	case _INTVAR_EVENT:
		event = &IntVarEvent{
			eventLogHeader: header,
		}
	case _BEGIN_LOAD_QUERY_EVENT:
		event = &BeginLoadQueryEvent{
			eventLogHeader: header,
		}
	case _APPEND_BLOCK_EVENT:
		event = &AppendBlockEvent{
			eventLogHeader: header,
		}
	case _EXECUTE_LOAD_QUERY_EVENT:
		event = &ExecuteLoadQueryEvent{
			eventLogHeader: header,
		}
	case _USER_VAR_EVENT:
		event = &UserVarEvent{
			eventLogHeader: header,
		}
	case _UNKNOWN_EVENT:
		event = &unknownEvent{
			eventLogHeader: header,
		}
	case _IGNORABLE_EVENT:
		event = &ignorableEvent{
			eventLogHeader: header,
		}
	case _HEARTBEAT_EVENT:
		event = &HeartBeatEvent{
			eventLogHeader: header,
		}
	case _STOP_EVENT:
		event = &StopEvent{
			eventLogHeader: header,
		}
	case _INCIDENT_EVENT:
		event = &IncidentEvent{
			eventLogHeader: header,
		}
	case _SLAVE_EVENT:
		event = &slaveEvent{
			eventLogHeader: header,
		}
	case _RAND_EVENT:
		event = &RandEvent{
			eventLogHeader: header,
		}
		//	case _TABLE_MAP_EVENT:
		//		event = &TableMapEvent{
		//			eventLogHeader: header,
		//		}
	default:
		println("Unknown event")
		println(fmt.Sprintf("% x\n", pack.buff))
		return nil, nil
	}

	ev.lastRotatePosition = header.NextPosition
	event.read(pack)
	return event, nil
}
