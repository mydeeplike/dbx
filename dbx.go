package dbx

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/mydeeplike/dbx/lib/syncmap"
	"io"
	"io/ioutil"
	"os"
	"reflect"
	"runtime/debug"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mattn/go-sqlite3"
)

// sql action:
const (
	ACTION_SELECT_ONE int = iota
	ACTION_SELECT_ALL
	ACTION_UPDATE
	ACTION_UPDATE_M
	ACTION_DELETE
	ACTION_INSERT
	ACTION_INSERT_IGNORE
	ACTION_REPLACE
	ACTION_COUNT
	ACTION_SUM
)

const (
	DRIVER_MYSQL int = iota
	DRIVER_SQLITE
)

const KEY_SEP string = "-"

//type Time struct {
//	time.Time
//}
//func (t *Time)String() string {
//	return t.Format("2006-01-02 15:04:05")
//}

var ErrNoRows = sql.ErrNoRows

func IsDup(err error) bool {
	if err != nil && strings.Index(err.Error(), "Duplicate") != -1 {
		return true
	}
	return false
}

func NoRows(err error) bool {
	if err == sql.ErrNoRows {
		return true
	}
	return false
}

func Check(err error) {
	if err != nil && err != sql.ErrNoRows {
		panic(err)
	}
}

func Now() string {
	t := time.Now()
	return t.Format("2006-01-02 15:04:05")
}

// 错误处理，仅用于 dbx 类，对外部透明，对外部仅返回标准的 error 和 panic()
type dbxError struct {
	data string
}

func (e dbxError) Error() string {
	return e.data
}
func dbxErrorNew(s string, args ...interface{}) *dbxError {
	e := &dbxError{data: fmt.Sprintf(s, args...)}
	return e
}
func dbxErrorType(err interface{}) bool {
	_, ok := err.(*dbxError)
	return ok
}
func dbxErrorDefer(err *error, db *Query) {
	if err1 := recover(); err1 != nil {
		*err = fmt.Errorf("dbx panic(): %v", err1)
		//return
		if !dbxErrorType(err1) {
			db.ErrorLog((*err).Error())
			debug.PrintStack()
			os.Exit(0)
		}
	}
}

//func dbxPanic(s string, args... interface{}) {
//	panic(dbxErrorNew(s, args...))
//}

type M []Map

type Map struct {
	Key   string
	Value interface{}
}

func (m M) toKeysValues() ([]string, []interface{}) {
	l := len(m)
	keys := make([]string, l)
	values := make([]interface{}, l)
	for k, v := range m {
		keys[k] = v.Key
		values[k] = v.Value
	}
	return keys, values
}

type Col struct {
	ColName     string // 列名: id
	FieldName   string // 结构体中的名字：Id
	FieldPos    []int  // 在结构体中的位置，支持嵌套 [1,0,-1,-1,-1]
	FieldStruct reflect.StructField
}

type ColFieldMap struct {
	fieldMap map[string]int
	fieldArr []string
	colMap   map[string]int
	colArr   []string
	cols     []*Col
}

func NewColFieldMap() *ColFieldMap {
	c := &ColFieldMap{}
	c.fieldMap = map[string]int{}
	c.colMap = map[string]int{}
	c.cols = []*Col{}
	return c

}
func (c *ColFieldMap) Add(col *Col) {
	if c.Exists(col.FieldName) {
		return
	}
	c.cols = append(c.cols, col)
	n := len(c.cols) - 1
	c.fieldMap[col.FieldName] = n
	c.fieldArr = append(c.fieldArr, col.FieldName)

	if col.ColName != "" {
		c.colMap[col.ColName] = n
		c.colArr = append(c.colArr, col.ColName)
	}
}

func (c *ColFieldMap) GetByColName(colName string) *Col {
	i, ok := c.colMap[colName]
	if !ok {
		return nil
	}
	return c.cols[i]
}

func (c *ColFieldMap) GetByFieldName(fieldName string) *Col {
	i, ok := c.fieldMap[fieldName]
	if !ok {
		return nil
	}
	return c.cols[i]
}

func (c *ColFieldMap) Exists(key string) bool {
	_, ok := c.fieldMap[key]
	return ok
}

type TableStruct struct {
	ColFieldMap   *ColFieldMap
	PrimaryKey    []string
	PrimaryKeyPos [][]int
	AutoIncrement string
	Type          reflect.Type
	EnableCache   bool
}

// pointerType 必须为约定值 &struct
func NewTableStruct(db *DB, tableName string, pointerType reflect.Type) (*TableStruct) {
	if pointerType.Kind() != reflect.Ptr {
		pointerType = reflect.New(pointerType).Type()
	}
	colFieldMap := NewColFieldMap()
	struct_fields_range_do(colFieldMap, pointerType, []int{})

	t := &TableStruct{}
	t.ColFieldMap = colFieldMap
	t.Type = pointerType
	t.PrimaryKey, t.AutoIncrement = get_table_info(db, tableName)
	t.EnableCache = false

	// 保存主键的位置
	t.PrimaryKeyPos = make([][]int, 0)
	for _, colName := range t.PrimaryKey {
		n := colFieldMap.colMap[colName]
		col := colFieldMap.cols[n]
		t.PrimaryKeyPos = append(t.PrimaryKeyPos, col.FieldPos)
	}
	return t
}

type DB struct {
	*sql.DB
	DriverType int
	Stdout     io.Writer
	Stderr     io.Writer

	// todo: 按照行缓存数据，只缓存主键条件的查询
	tableStruct      map[string]*TableStruct
	tableData        map[string]*syncmap.Map
	tableEnableCache bool

	readOnly bool // 只读模式，禁止写，防止出错。
}

type Query struct {
	*DB
	table  string
	fields []string // SELECT

	primaryKeyStr string
	primaryArgs   []interface{} // 主键的值

	where     string
	whereArgs []interface{}
	whereM    M

	orderBy M

	limitStart int64
	limitEnd   int64

	updateFields []string
	updateArgs   []interface{} // 存储参数
}

func OpenFile(filePath string) *os.File {
	fp, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		panic(err)
	}
	return fp
}

func Open(driverName, dataSourceName string) (*DB, error) {
	db, err := sql.Open(driverName, dataSourceName)
	if err != nil {
		return nil, err
	}
	var driverType int
	if driverName == "mysql" {
		driverType = DRIVER_MYSQL
	} else {
		driverType = DRIVER_SQLITE
	}
	//cacheTable := &map[string][]string{}
	//cacheData := &map[string]*map[string]interface{}{}
	return &DB{
		DB:               db,
		DriverType:       driverType,
		Stdout:           ioutil.Discard,
		Stderr:           os.Stderr,
		tableStruct:      make(map[string]*TableStruct),
		tableData:        make(map[string]*syncmap.Map), // 第一级的 map 会在启动的时候初始化好，第二级的使用安全 map
		tableEnableCache: false,
	}, err
}

func sql2str(sqlstr string, args ...interface{}) string {
	sqlstr = strings.Replace(sqlstr, "?", "%v", -1)
	sql1 := fmt.Sprintf(sqlstr, args...)
	return sql1
}

func (db *DB) Log(s string, args ...interface{}) {
	if db.Stdout == nil || db.Stdout == ioutil.Discard {
		return
	}
	str := fmt.Sprintf(s, args...)
	fmt.Fprintf(db.Stdout, "[%v] %v\n", Now(), str)
}
func (db *DB) LogSQL(s string, args ...interface{}) {
	str := sql2str(s, args...)
	db.Log(str)
}
func (db *DB) ErrorLog(s string, args ...interface{}) {
	if db.Stderr == nil || db.Stderr == ioutil.Discard {
		return
	}
	str := fmt.Sprintf(s, args...)
	fmt.Fprintf(db.Stderr, "[%v] %v\n", Now(), str)
}

func (db *DB) ErrorSQL(s string, sql1 string, args ...interface{}) {
	if db.Stderr == nil || db.Stderr == ioutil.Discard {
		return
	}
	str := fmt.Sprintf(s)
	sql2 := sql2str(sql1, args...)
	fmt.Fprintf(db.Stderr, "[%v] %v %v\n", Now(), sql2, str)
}

func (db *DB) Panic(s string, args ...interface{}) {
	db.ErrorLog(s, args...)
	panic(dbxErrorNew(s, args...))
}

// ifc 如果不是指针类型，则 new 出指针类型，方便使用 type / &type
func (db *DB) Bind(tableName string, ifc interface{}, enableCache bool) {
	t := reflect.TypeOf(ifc)
	if t.Kind() == reflect.Struct {
		t = reflect.New(t).Type()
	}
	tableStruct, ok := db.tableStruct[tableName]
	if !ok {
		tableStruct := NewTableStruct(db, tableName, t)
		tableStruct.EnableCache = enableCache
		db.tableStruct[tableName] = tableStruct
	} else {
		tableStruct.EnableCache = enableCache
	}
}

func (db *DB) SetReadOnly(b bool) {
	db.readOnly = b
}

func (db *DB) EnableCache(b bool) {
	db.tableEnableCache = b
	if b == true && len(db.tableData) == 0 {
		db.LoadCache()
	}
}

func (db *DB) loadTableCache(tableName string) {
	tableStruct := db.tableStruct[tableName]
	list := reflect_make_slice_pointer(tableStruct.Type)
	err := db.Table(tableName).All(list)
	if err != nil && err != sql.ErrNoRows {
		panic(err)
	}

	listValue := reflect.ValueOf(list).Elem()

	//mp := &syncmap.Map{}
	mp := new(syncmap.Map)
	for i := 0; i < listValue.Len(); i++ {
		row := listValue.Index(i)
		pkKey := get_pk_key(tableStruct, row.Elem())
		mp.Store(pkKey, row.Interface())
	}
	db.tableData[tableName] = mp
}

func (db *DB) LoadCache() {
	if db.tableEnableCache == false {
		return
	}
	for tableName, tableStruct := range db.tableStruct {
		if tableStruct.EnableCache == false {
			continue
		}
		db.loadTableCache(tableName)
	}
}

func (db *DB) Table(name string) *Query {
	q := &Query{
		DB:          db,
		table:       name,
		fields:      []string{},
		whereArgs:   []interface{}{},
		primaryArgs: []interface{}{},
		updateArgs:  []interface{}{},
		orderBy:     M{},
	}
	return q
}

func (q *Query) Bind(ifc interface{}, enableCache bool) {
	tableName := q.table
	t := reflect.TypeOf(ifc)
	if t.Kind() == reflect.Struct {
		t = reflect.New(t).Type()
	}
	tableStruct, ok := q.tableStruct[tableName]
	if !ok {
		tableStruct := NewTableStruct(q.DB, tableName, t)
		tableStruct.EnableCache = enableCache
		q.tableStruct[tableName] = tableStruct
	} else {
		tableStruct.EnableCache = enableCache
	}
}

func (q *Query) LoadCache() {
	if q.tableEnableCache == false {
		return
	}
	tableName := q.table
	tableStruct := q.tableStruct[tableName]
	if tableStruct.EnableCache == false {
		return
	}
	q.loadTableCache(q.table)
}

func (q *Query) AllFromCache() *syncmap.Map {
	v, ok := q.tableData[q.table]
	if ok {
		return v
	} else {
		return nil
	}
}

func (q *Query) Fields(fields ...string) *Query {
	q.fields = fields
	return q
}

/*
	dbx.M{{"uid", 1}, {"gid", -1}}
*/
//func (q *Query) OrderBy(m M) *Query {
//	q.orderBy = m
//	return q
//}
//
//func args_time_format(args []interface{}) {
//	for k, v := range args {
//		if _, ok := v.(time.Time); ok {
//			args[k] = v.(time.Time).Format("2006-01-02 15:04:05")
//		}
//	}
//}
//
//func FormatTime(t time.Time) string {
//	return t.Format("2006-01-02 15:04:05")
//}

func (q *Query) Where(str string, args ...interface{}) *Query {
	//args_time_format(args)
	if q.where == "" {
		q.where = str
	} else {
		q.where += " AND " + str
	}
	q.whereArgs = append(q.whereArgs, args...)
	return q
}

func (q *Query) And(str string, args ...interface{}) *Query {
	return q.Where(str, args...)
}

func (q *Query) Or(str string, args ...interface{}) *Query {
	if q.where == "" {
		q.where = str
	} else {
		q.where += " OR " + str
	}
	q.whereArgs = append(q.whereArgs, args...)
	return q
}

func (q *Query) WhereM(m M) *Query {
	q.whereM = append(q.whereM, m...)
	/*
		l := len(m)
		whereCols := make([]string, l)
		args := make([]interface{}, l)
		for i, mp := range m {
			args[i] = mp.Value
			whereCols[i] = mp.Key
		}
		q.whereArgs = args
		q.where = arr_to_sql_add(whereCols, "=?", " AND ")
	*/
	return q
}

func (q *Query) WherePK(args ...interface{}) *Query {
	q.primaryArgs = args

	//q.primaryArgs = args

	//str := arr_to_sql_add(args, "-", "")

	str := ""
	for _, v := range args {
		str += fmt.Sprintf("%v", v) + KEY_SEP
	}
	str = strings.TrimRight(str, KEY_SEP)
	q.primaryKeyStr = str

	/*
		q.primaryArgs = args
		str := ""
		for _, v := range args {
			str = fmt.Sprintf("%v-", v)
		}
		str = strings.TrimRight(str, "-")
		q.primaryKeyStr = str

		// 覆盖 where 条件！避免冲突
		q.whereArgs = args

		//todo: 性能可以继续优化，提高一倍的速度！
		tableStruct, _ := q.getTableStruct()
		q.where = arr_to_sql_add(tableStruct.PrimaryKey, "=?", ",")
	*/
	return q
}

// 合并 where 条件
//func (q *Query) wherePKToKey() (string) {
//	str := ""
//	for _, v := range args {
//		str = fmt.Sprintf("%v-", v)
//	}
//	str = strings.TrimRight(str, "-")
//	q.primaryKeyStr = str
//}

func (q *Query) whereToSQL(tableStruct *TableStruct) (where string, args []interface{}) {

	// 主键优先级最高，独占
	if len(q.primaryArgs) > 0 {
		where = " WHERE " + arr_to_sql_add(tableStruct.PrimaryKey, "=?", " AND ")
		args = q.primaryArgs
		return
	}

	// 合并所有的 where + whereM 条件
	where, args = q.whereToSQLDo()
	return
}

func (q *Query) whereToSQLDo() (where string, args []interface{}) {
	// 合并所有的 where + whereM 条件
	where = q.where
	args = q.whereArgs
	if len(q.whereM) > 0 {
		colNames, args2 := q.whereM.toKeysValues()
		whereAdd := arr_to_sql_add(colNames, "=?", " AND ")
		if where == "" {
			where = whereAdd
		} else {
			where += " AND " + whereAdd
		}
		args = append(args, args2...)
	}

	if where != "" {
		where = " WHERE " + where
	}
	return
}


func (q *Query) Sort(colName string, order int) *Query {
	q.orderBy = append(q.orderBy, Map{colName, order})
	return q
}

func (q *Query) SortM(m M) *Query {
	for _, v := range m {
		q.orderBy = append(q.orderBy, Map{v.Key, v.Value})
	}
	return q
}

/*
	Limit(10)
	Limit(0, 10)
*/
func (q *Query) Limit(limitStart int64, limitEnds ...int64) *Query {
	limitEnd := int64(0)
	if len(limitEnds) > 0 {
		limitEnd = limitEnds[0]
	}
	q.limitStart = limitStart
	q.limitEnd = limitEnd
	return q
}

func (q *Query) orderByToSQL() string {
	sqlAdd := ""
	for _, m := range q.orderBy {
		k := m.Key
		v, ok := m.Value.(int)
		if !ok {
			sqlAdd += fmt.Sprintf("%v %v AND", k, "ASC")
			continue
		}
		if v == 1 {
			sqlAdd += fmt.Sprintf("%v %v AND", k, "ASC")
		} else if v == -1 {
			sqlAdd += fmt.Sprintf("%v %v AND", k, "DESC")
		}
	}
	return strings.TrimRight(sqlAdd, " AND")
}

// 将当前条件转化为 SQL 语句
func (q *Query) toSQL(tableStruct *TableStruct, action int, rvalues ...reflect.Value) (sql1 string, args []interface{}) {
	fields := "*"
	where := ""
	orderBy := ""
	limit := ""
	if len(q.fields) > 0 {
		fields = strings.Join(q.fields, ",")
	}

	where, args = q.whereToSQL(tableStruct)

	if len(q.orderBy) > 0 {
		orderBy = " ORDER BY " + q.orderByToSQL()
	}
	if q.limitStart != 0 || q.limitEnd != 0 {
		if q.limitEnd == 0 {
			limit = fmt.Sprintf(" LIMIT %v", q.limitStart)
		} else {
			limit = fmt.Sprintf(" LIMIT %v,%v", q.limitStart, q.limitEnd)
		}
	}
	switch action {
	case ACTION_SELECT_ONE:
		limit = " LIMIT 1"
		sql1 = fmt.Sprintf("SELECT %v FROM %v%v%v%v", fields, q.table, where, orderBy, limit)
	case ACTION_SELECT_ALL:
		sql1 = fmt.Sprintf("SELECT %v FROM %v%v%v%v", fields, q.table, where, orderBy, limit)
	case ACTION_UPDATE:
		if q.DriverType == DRIVER_MYSQL {
			limit = " LIMIT 1"
		}

		var updateArgs []interface{}
		updateArgs, pkArgs, _ := struct_value_to_args(tableStruct, rvalues[0], true, true)

		// todo: 去掉主键的更新
		colNames := array_sub(tableStruct.ColFieldMap.colArr, tableStruct.PrimaryKey)
		updateFields := arr_to_sql_add(colNames, "=?", ",")
		if where == "" {
			where = " WHERE " + arr_to_sql_add(tableStruct.PrimaryKey, "=?", " AND ")
			args = append(args, pkArgs...)
		}
		sql1 = fmt.Sprintf("UPDATE %v SET %v%v%v", q.table, updateFields, where, limit)
		args = append(updateArgs, args...)
	case ACTION_UPDATE_M:
		if q.DriverType == DRIVER_SQLITE {
			limit = ""
		}
		colNames := arr_to_sql_add(q.updateFields, "=?", ",")
		sql1 = fmt.Sprintf("UPDATE %v SET %v%v%v", q.table, colNames, where, limit)
		args = append(q.updateArgs, args...)
	case ACTION_DELETE:
		if q.DriverType == DRIVER_SQLITE {
			limit = ""
		}
		sql1 = fmt.Sprintf("DELETE FROM %v%v%v", q.table, where, limit)
	case ACTION_INSERT:
		uncludes := []string{tableStruct.AutoIncrement}
		colNames := array_sub(tableStruct.ColFieldMap.colArr, uncludes)
		fields := arr_to_sql_add(colNames, "", ",")
		values := strings.TrimRight(strings.Repeat("?,", len(colNames)), ",")
		sql1 = fmt.Sprintf("INSERT INTO %v (%v) VALUES (%v)", q.table, fields, values)
		args, _, _ = struct_value_to_args(tableStruct, rvalues[0], true, false)
	case ACTION_INSERT_IGNORE:
		// copy from ACTION_INSERT
		uncludes := []string{tableStruct.AutoIncrement}
		colNames := array_sub(tableStruct.ColFieldMap.colArr, uncludes)
		fields := arr_to_sql_add(colNames, "", ",")
		values := strings.TrimRight(strings.Repeat("?,", len(colNames)), ",")
		if q.DriverType == DRIVER_MYSQL {
			sql1 = fmt.Sprintf("INSERT IGNORE INTO %v (%v) VALUES (%v)", q.table, fields, values)
		} else if q.DriverType == DRIVER_SQLITE {
			sql1 = fmt.Sprintf("INSERT OR IGNORE INTO %v (%v) VALUES (%v)", q.table, fields, values)
		}
		args, _, _ = struct_value_to_args(tableStruct, rvalues[0], true, false)
		// copy end
	case ACTION_REPLACE:
		tableStruct := q.tableStruct[q.table]
		fields := arr_to_sql_add(tableStruct.ColFieldMap.colArr, "", ",")
		values := strings.TrimRight(strings.Repeat("?,", len(tableStruct.ColFieldMap.colArr)), ",")
		sql1 = fmt.Sprintf("REPLACE INTO %v (%v) VALUES (%v)", q.table, fields, values)
		args, _, _ = struct_value_to_args(tableStruct, rvalues[0], false, false)
	case ACTION_COUNT:
		sql1 = fmt.Sprintf("SELECT COUNT(*) FROM %v%v", fields, q.table, where)
	case ACTION_SUM:
	}
	return
}

func (q *Query) One(arrIfc interface{}) (err error) {
	defer dbxErrorDefer(&err, q)
	arrType := reflect.TypeOf(arrIfc)
	arrValue := reflect.ValueOf(arrIfc)
	if arrType.Kind() != reflect.Ptr {
		errStr := fmt.Sprintf("must pass a struct pointer: %v", arrType.Kind())
		panic(dbxErrorNew(errStr))
	}

	arr := arrType.Elem() // 求一级指针，&arr -> arr
	if arr.Kind() != reflect.Struct {
		errStr := fmt.Sprintf("must pass a struct pointer: %v", arr.Kind())
		panic(dbxErrorNew(errStr))
	}

	// 如果没有 Bind() ，这里就会执行下去，从缓存里读表结构，不用每次都反射，提高效率
	tableStruct := q.getTableStruct(arrType)

	// 判断是否开启了缓存
	if q.tableEnableCache && tableStruct.EnableCache && len(q.primaryArgs) > 0 {
		if len(q.primaryKeyStr) != 0 {
			mp, ok := q.tableData[q.table]
			if !ok {
				errStr := fmt.Sprintf("q.tableData[q.table]: key %v does not exists.", q.table)
				q.ErrorLog(errStr)
				return errors.New(errStr)
			}
			ifc, ok := mp.Load(q.primaryKeyStr)
			if ok {
				arrValue.Elem().Set(reflect.ValueOf(ifc).Elem())
				return nil
			} else {
				// 只要开启 cache，必须终止！提高速度！
				return ErrNoRows
			}
		} else {
			return ErrNoRows
		}
	}

	// var stmt *sql.Stmt
	// var rows *sql.Rows
	sql1, args := q.toSQL(tableStruct, ACTION_SELECT_ONE)
	stmt, err := q.Prepare(sql1)
	if err != nil {
		q.ErrorSQL(err.Error(), sql1, args...)
		return err
	}
	defer stmt.Close()
	rows, err := stmt.Query(args...)
	q.LogSQL(sql1, args...)
	if err != nil {
		q.ErrorSQL(err.Error(), sql1, args...)
		return err
	}
	defer rows.Close()

	// 数据库返回的列，需要和表结构进行对应
	columns, err := rows.Columns()
	if err != nil {
		q.ErrorSQL(err.Error(), sql1, args...)
		return err
	}
	posMap := map[int][]int{}
	for k, colName := range columns {
		n, ok := tableStruct.ColFieldMap.colMap[colName]
		if !ok {
			continue
		}
		col := tableStruct.ColFieldMap.cols[n]
		posMap[k] = col.FieldPos
	}

	values := make([]interface{}, len(columns))
	for i := range values {
		values[i] = new(interface{})
	}

	if !rows.Next() {
		return sql.ErrNoRows
	}
	err = rows.Scan(values...)
	if err != nil {
		q.ErrorSQL(err.Error(), sql1, args...)
		return err
	}
	// 对应到相应的列
	for k, _ := range columns {
		pos, ok := posMap[k]
		if !ok {
			continue
		}

		//ifc_pos_to_value(values[k], pos, arrValue)
		ifc := *(values[k].(*interface{})) // db 里面取出来的数据
		//valueV := reflect.ValueOf(value)
		//valueKind := valueV.Kind()
		col := get_reflect_value_from_pos(arrValue.Elem(), pos) // 需要设置的字段

		set_value_to_ifc(col, ifc)

	}

	err = rows.Err()
	if err != nil {
		q.ErrorSQL(err.Error(), sql1, args...)
		return err
	}
	return nil
}

func (q *Query) All(arrListIfc interface{}) (err error) {
	defer dbxErrorDefer(&err, q)
	arrListType := reflect.TypeOf(arrListIfc)
	if arrListType.Kind() != reflect.Ptr {
		q.Panic("must pass a slice pointer: %v", arrListType.Kind())
	}

	arrlist := arrListType.Elem() // 求一级指针，&arrlist -> arrlist
	if arrlist.Kind() != reflect.Slice {
		q.Panic("must pass a slice pointer: %v", arrListType.Kind())
	}

	arrType := arrlist.Elem() // &struct
	arrIsPtr := (arrType.Kind() == reflect.Ptr)

	arrListValue := reflect.ValueOf(arrListIfc).Elem()
	// 如果没有 Bind() ，这里就会执行下去，从缓存里读表结构，不用每次都反射，提高效率
	tableStruct := q.getTableStruct(arrType)

	//var stmt *sql.Stmt
	//var rows *sql.Rows

	// 判断是否为 whereM
	sql1, args := q.toSQL(tableStruct, ACTION_SELECT_ALL)
	stmt, err := q.Prepare(sql1)
	if err != nil {
		q.ErrorSQL(err.Error(), sql1, args...)
		return err
	}
	defer stmt.Close()
	rows, err := stmt.Query(args...)
	q.LogSQL(sql1, args...)
	if err != nil {
		q.ErrorSQL(err.Error(), sql1, args...)
		return err
	}
	defer rows.Close()


	err = rows_to_arr_list(arrListValue, rows, tableStruct, arrIsPtr) // 这个错误要保留
	if err != nil {
		return err
	}
	return nil
}

func (q *Query) Count() (n int64, err error) {
	defer dbxErrorDefer(&err, q)

	// 判断 WHERE 条件是否为空
	if q.tableEnableCache {
		tableStruct := q.getTableStruct()
		if tableStruct.EnableCache && q.where == "" && len(q.whereM) == 0 {
			return q.tableData[q.table].Len(), nil
		}
	}
	q.Fields("COUNT(*)")
	sql1, args := q.toSQL(nil, ACTION_SELECT_ONE)
	err = q.QueryRow(sql1, args...).Scan(&n)
	q.LogSQL(sql1, args...)
	if err != nil {
		q.ErrorSQL(err.Error(), sql1, args...)
		return
	}
	return
}

// 针对某一列
func (q *Query) Sum(colName string) (n int64, err error) {
	defer dbxErrorDefer(&err, q)
	var n2 sql.NullInt64
	q.Fields("SUM(" + colName + ")")
	sql1, args := q.toSQL(nil, ACTION_SELECT_ONE)
	err = q.QueryRow(sql1, args...).Scan(&n2)
	q.LogSQL(sql1, args...)
	if err != nil {
		q.ErrorSQL(err.Error(), sql1, args...)
		return
	}
	n = n2.Int64
	return
}

// 针对某一列
func (q *Query) Max(colName string) (n int64, err error) {
	defer dbxErrorDefer(&err, q)
	var n2 sql.NullInt64
	q.Fields("MAX(" + colName + ")")
	sql1, args := q.toSQL(nil, ACTION_SELECT_ONE)
	err = q.QueryRow(sql1, args...).Scan(&n2)
	q.LogSQL(sql1, args...)
	if err != nil {
		q.ErrorSQL(err.Error(), sql1, args...)
		return
	}
	n = n2.Int64
	return
}

// 针对某一列
func (q *Query) Min(colName string) (n int64, err error) {
	defer dbxErrorDefer(&err, q)
	var n2 sql.NullInt64
	q.Fields("MIN(" + colName + ")")
	sql1, args := q.toSQL(nil, ACTION_SELECT_ONE)
	err = q.QueryRow(sql1, args...).Scan(&n2)
	q.LogSQL(sql1, args...)
	if err != nil {
		// Scan error on column index 0, name "MIN(blockid)": converting driver.Value type <nil> ("<nil>") to a int64

		q.ErrorSQL(err.Error(), sql1, args...)
		return
	}
	n = n2.Int64
	return
}

func (q *Query) Truncate() (err error) {
	defer dbxErrorDefer(&err, q)

	// 判断 WHERE 条件是否为空
	if q.tableEnableCache {
		q.tableData[q.table] = new(syncmap.Map)
	}

	sql1 := ""
	if q.DriverType == DRIVER_MYSQL {
		sql1 = "TRUNCATE "+q.table
	} else {
		sql1 = "DELETE FROM "+q.table
	}
	_, err = q.Exec(sql1)
	if err != nil {
		q.ErrorSQL(err.Error(), sql1)
	}
	return
}


// arrType 必须为 &struct
func (q *Query) getTableStruct(arrTypes ...reflect.Type) (tableStruct *TableStruct) {
	if len(arrTypes) == 0 {
		tableStruct, ok := q.tableStruct[q.table]
		if !ok {
			return nil
			//panic(dbxErrorNew("q.tableStruct[q.table] does not exists:" + q.table))
		} else {
			return tableStruct
		}
	}
	arrType := arrTypes[0]
	if arrType.Kind() != reflect.Ptr {
		arrType = reflect.New(arrType).Type()
		//panic(dbxErrorNew("getTableStruct(arrType) expect type of &struct."))
	}

	var ok bool
	tableStruct, ok = q.tableStruct[q.table]
	if !ok {
		tableStruct = NewTableStruct(q.DB, q.table, arrType)
		q.tableStruct[q.table] = tableStruct
	}
	return
}



// ifc 最好为 &struct
func (q *Query) Insert(ifc interface{}) (insertId int64, err error) {
	return q.insert_replace(ifc, false, false)
}

// ifc 最好为 &struct
func (q *Query) Replace(ifc interface{}) (insertId int64, err error) {
	return q.insert_replace(ifc, true, false)
}

// ifc 最好为 &struct
func (q *Query) InsertIgnore(ifc interface{}) (insertId int64, err error) {
	return q.insert_replace(ifc, false, true)
}

func (q *Query) insert_replace(ifc interface{}, isReplace bool, ignore bool) (insertId int64, err error) {
	if q.readOnly {
		return
	}

	defer dbxErrorDefer(&err, q)

	ifc2 := ifc
	arrType := reflect.TypeOf(ifc)
	arrValue := reflect.ValueOf(ifc)
	ifcValueP := arrValue
	arrTypeElem := arrType
	if arrType.Kind() == reflect.Ptr {
		arrTypeElem = arrType.Elem()
	} else {
		ifcValueP = reflect.New(reflect.TypeOf(ifc))
		ifcValueP.Elem().Set(arrValue)
		ifc2 = ifcValueP.Interface()
	}

	if arrValue.Kind() == reflect.Ptr {
		arrValue = arrValue.Elem()
	}

	if arrTypeElem.Kind() != reflect.Struct {
		q.Panic("must pass a struct or struct pointer: %v", arrTypeElem.Kind())
	}

	// 如果没有 Bind() ，这里就会执行下去，从缓存里读表结构，不用每次都反射，提高效率
	var tableStruct *TableStruct
	tableStruct = q.getTableStruct(arrType)

	action := ACTION_INSERT
	if ignore {
		action = ACTION_INSERT_IGNORE
	} else if isReplace {
		action = ACTION_REPLACE
	}

	var sql1 string
	var args []interface{}
	sql1, args = q.toSQL(tableStruct, action, arrValue)

	if ignore {
		_, err = q.DB.Exec(sql1, args...)
		q.LogSQL(sql1, args...)
		if err != nil {
			errStr := err.Error()
			errStrLower := strings.ToLower(errStr)
			if !strings.Contains(errStrLower, "unique") && !strings.Contains(errStrLower, "duplicate") {
				q.ErrorSQL(errStr, sql1, args...)
			}
		}
		return
	}

	insertId, err = q.Exec(sql1, args...)

	//var result sql.Result
	//result, err = q.Exec(sql1, args...)
	//q.LogSQL(sql1, args...)
	//
	//if err != nil {
	//	q.ErrorSQL(err.Error(), sql1, args...)
	//	return
	//}
	//insertId, err = result.LastInsertId()
	//if err != nil {
	//	q.ErrorSQL(err.Error(), sql1, args...)
	//	return
	//}

	// cache
	if !ignore && q.tableEnableCache && tableStruct.EnableCache {
		mp, ok := q.tableData[q.table]
		if !ok {
			errStr := fmt.Sprintf("q.tableData[q.table]: key %v does not exists.", q.table)
			q.ErrorLog(errStr)
			err = errors.New(errStr)
			return
		}

		// update auto_increment value
		if tableStruct.AutoIncrement != "" && !isReplace {
			n, ok := tableStruct.ColFieldMap.colMap[tableStruct.AutoIncrement]
			pos := tableStruct.ColFieldMap.cols[n].FieldPos
			if !ok {
				errStr := fmt.Sprintf("auto_increment not found: %v.%v", q.table, tableStruct.AutoIncrement)
				q.ErrorLog(errStr)
				err = errors.New(errStr)
				return
			}
			colValue := get_reflect_value_from_pos(ifcValueP, pos)

			set_value_to_ifc(colValue, insertId)

			//ifc2 = ifcValueP.Interface()
		}
		pkkey := get_pk_key(tableStruct, arrValue)
		mp.Store(pkkey, ifc2)
	}

	return
}

func (q *Query) Update(ifc interface{}) (affectedRows int64, err error) {
	if q.readOnly {
		return
	}

	defer dbxErrorDefer(&err, q)

	ifc2 := ifc
	arrType := reflect.TypeOf(ifc)
	arrValue := reflect.ValueOf(ifc)
	ifcValueP := arrValue
	arrTypeElem := arrType
	if arrType.Kind() == reflect.Ptr {
		arrTypeElem = arrType.Elem()
	} else {
		ifcValueP = reflect.New(reflect.TypeOf(ifc))
		ifcValueP.Elem().Set(arrValue)
		ifc2 = ifcValueP.Interface()
	}

	if arrValue.Kind() == reflect.Ptr {
		arrValue = arrValue.Elem()
	}

	if arrTypeElem.Kind() != reflect.Struct {
		errStr := fmt.Sprintf("must pass a struct or struct pointer: %v", arrTypeElem.Kind())
		q.ErrorLog(errStr)
		err = errors.New(errStr)
		return
	}

	// 如果没有 Bind() ，这里就会执行下去，从缓存里读表结构，不用每次都反射，提高效率
	var tableStruct *TableStruct
	tableStruct = q.getTableStruct(arrType)

	var sql1 string
	var args []interface{}
	sql1, args = q.toSQL(tableStruct, ACTION_UPDATE, arrValue)


	affectedRows, err = q.Exec(sql1, args...)
	//var result sql.Result
	//result, err = q.Exec(sql1, args...)
	//q.LogSQL(sql1, args...)
	//if err != nil {
	//	q.ErrorSQL(err.Error(), sql1, args...)
	//	return
	//}
	//affectedRows, err = result.RowsAffected()
	//if err != nil {
	//	q.ErrorSQL(err.Error(), sql1, args...)
	//	return
	//}

	// cache
	if q.tableEnableCache && tableStruct.EnableCache {
		// 判断是否通过主键更新，如果是主键则只更新
		pkkey := get_pk_key(tableStruct, arrValue)
		// todo: 修正为主键的值？还是报错？
		mp, ok := q.tableData[q.table]
		if !ok {
			errStr := fmt.Sprintf("q.tableData[q.table]: key %v does not exists.", q.table)
			q.ErrorLog(errStr)
			err = errors.New(errStr)
			return
		}
		//v, ok := mp.Load("1")
		//mp.Range(func(k interface{}, v interface{}) bool {
		//	fmt.Printf("%v: %v\n", k, v)
		//	return true
		//})
		//fmt.Printf("Len: %v, 1: %v", mp.Len(), v)
		mp.Store(pkkey, ifc2)
	}

	return
}

func (q *Query) UpdateM(m M) (affectedRows int64, err error) {
	if q.readOnly {
		return
	}

	defer dbxErrorDefer(&err, q)

	// 如果开启了缓存，则先查询，再更新，否则，更新完以后，就查询不到了！
	tableStruct := q.getTableStruct()

	updateFields := make([]string, len(m))
	updateArgs := make([]interface{}, len(m))
	for i, m := range m {
		if in_array(m.Key, tableStruct.PrimaryKey) {
			//return 0, errors.New("you can't update primary key, you can remove it first.")
			continue
		}
		updateFields[i] = m.Key
		updateArgs[i] = m.Value
	}
	q.updateFields = updateFields
	q.updateArgs = updateArgs

	pkColNames := tableStruct.PrimaryKey

	// 更新缓存，不用判断条数，反正都是针对小表缓存
	if q.tableEnableCache && tableStruct.EnableCache {
		var rows *sql.Rows
		var stmt *sql.Stmt
		// 只是选择主键
		fields2 := arr_to_sql_add(append(pkColNames), "", ",")
		where2, args2 := q.whereToSQL(tableStruct)
		sql2 := fmt.Sprintf("SELECT %v FROM %v%v", fields2, q.table, where2)
		stmt, err = q.Prepare(sql2)
		if err != nil {
			q.ErrorSQL(err.Error(), sql2, args2...)
			return
		}
		defer stmt.Close()
		rows, err = stmt.Query(args2...)
		q.LogSQL(sql2, args2...)
		if err != nil {
			q.ErrorSQL(err.Error(), sql2, args2...)
			return
		}
		defer rows.Close()

		ifc := reflect_make_slice_pointer(tableStruct.Type)
		listValue := reflect.ValueOf(ifc)
		err = rows_to_arr_list(listValue, rows, tableStruct, true) // 保留错误

		// 遍历 arrListType
		//listValue := reflect.ValueOf(arrListIfc).Elem()
		mp, ok := q.tableData[q.table];
		if !ok {
			errStr := fmt.Sprintf("q.tableData[q.table]: key %v does not exists.", q.table)
			q.ErrorLog(errStr)
			err = errors.New(errStr)
			return
		}

		poses := make([][]int, len(updateFields))
		for k, colName := range updateFields {
			n, ok := tableStruct.ColFieldMap.colMap[colName]
			if !ok {
				q.ErrorLog("UpdateM() colNmae does not exists: "+colName)
				continue
			}
			col := tableStruct.ColFieldMap.cols[n]
			poses[k] = col.FieldPos
		}

		listValue = listValue.Elem()
		for i := 0; i < listValue.Len(); i++ {
			row := listValue.Index(i) // 只有主键的数据
			pkKey := get_pk_key(tableStruct, row.Elem())

			// 遍历 M，挨个更新字段
			old, ok := mp.Load(pkKey)
			if !ok {
				continue
				// todo: caceh 与 db 数据不一致，应该修补，但是数据不完整，跳过
				// mp[pkKey] = row.Interface()
			} else {
				// 更新有限的字段
				//fmt.Printf("old: %#v\n", old)
				for j, _ := range updateFields {
					pos := poses[j]
					var oldV reflect.Value
					oldV = get_reflect_value_from_pos(reflect.ValueOf(old).Elem(), pos)
					set_value_to_ifc(oldV, updateArgs[j])
					//oldV.Set(reflect.ValueOf(updateArgs[j]))
				}
			}
		}
	}

	// 更新
	var sql1 string
	var args []interface{}
	sql1, args = q.toSQL(tableStruct, ACTION_UPDATE_M)

	affectedRows, err = q.Exec(sql1, args...)

	//result, err = q.Exec(sql1, args...)
	//q.LogSQL(sql1, args...)
	//if err != nil {
	//	q.ErrorSQL(err.Error(), sql1, args...)
	//	return
	//}
	//affectedRows, err = result.RowsAffected()
	//if err != nil {
	//	q.ErrorSQL(err.Error(), sql1, args...)
	//	return
	//}

	return
}

func (db *DB) Exec(sql1 string, args ... interface{}) (n int64, err error) {
	var result sql.Result
	result, err = db.DB.Exec(sql1, args...)
	db.LogSQL(sql1, args...)
	if err != nil {
		db.ErrorSQL(err.Error(), sql1, args...)
		return
	}
	prefix := strings.ToUpper(sql1[0:6])
	if prefix == "INSERT" {
		n, err = result.LastInsertId()
		if err != nil {
			db.ErrorSQL(err.Error(), sql1, args...)
			return
		}
	} else if prefix == "UPDATE" || prefix == "DELETE" {
		n, err = result.RowsAffected()
		if err != nil {
			db.ErrorSQL(err.Error(), sql1, args...)
			return
		}
	}
	return
}

func (q *Query) Delete() (n int64, err error) {
	if q.readOnly {
		return
	}

	defer dbxErrorDefer(&err, q)

	// 更新缓存
	tableStruct := q.getTableStruct()
	if q.tableEnableCache && tableStruct.EnableCache {

		where2, args2 := q.whereToSQL(tableStruct)

		mp, ok := q.tableData[q.table]
		if !ok {
			errStr := fmt.Sprintf("q.tableData[q.table]: key %v does not exists.", q.table)
			q.ErrorLog(errStr)
			err = errors.New(errStr)
			return
		}

		// 删除所有
		if where2 == "" {
			q.LoadCache()
			// 根据主键删除
		} else if len(q.primaryKeyStr) != 0 {
			mp.Delete(q.primaryKeyStr)
		} else {
			// 根据条件查找，删除，类似 update
			var rows *sql.Rows
			var stmt *sql.Stmt
			pkColNames := tableStruct.PrimaryKey
			fields2 := arr_to_sql_add(append(pkColNames), "", ",")
			sql2 := fmt.Sprintf("SELECT %v FROM %v%v", fields2, q.table, where2)
			stmt, err = q.Prepare(sql2)
			if err != nil {
				q.ErrorSQL(err.Error(), sql2, args2...)
				return
			}
			defer stmt.Close()
			rows, err = stmt.Query(args2...)
			q.LogSQL(sql2, args2...)
			if err != nil {
				q.ErrorSQL(err.Error(), sql2, args2...)
				return
			}
			defer rows.Close()

			ifc := reflect_make_slice_pointer(tableStruct.Type)
			listValue := reflect.ValueOf(ifc)
			err = rows_to_arr_list(listValue, rows, tableStruct, true) // 保留错误

			// 遍历 arrListType
			//listValue := reflect.ValueOf(arrListIfc).Elem()
			listValue = listValue.Elem()
			for i := 0; i < listValue.Len(); i++ {
				row := listValue.Index(i)
				pkKey := get_pk_key(tableStruct, row.Elem())
				mp.Delete(pkKey)
			}
			//q.tableData[q.table] = mp
		}

	}

	var sql1 string
	var args []interface{}
	sql1, args = q.toSQL(tableStruct, ACTION_DELETE)


	n, err = q.Exec(sql1, args...)

	//var result sql.Result
	//result, err = q.Exec(sql1, args...)
	//q.LogSQL(sql1, args...)
	//if err != nil {
	//	q.ErrorSQL(err.Error(), sql1, args...)
	//	return
	//}
	//
	//n, err = result.RowsAffected()
	//if err != nil {
	//	q.ErrorSQL(err.Error(), sql1, args...)
	//	return
	//}

	return
}

func (db *DB) DebugCache() {
	for tableName, mp := range db.tableData {
		fmt.Printf("=================== %v ==================\n", tableName)
		mp.Range(func(k, v interface{}) bool {
			fmt.Printf("%v: %+v\n", k, v)
			return true
		})
		fmt.Printf("=====================================\n", tableName)
	}
}
