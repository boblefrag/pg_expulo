// pg_expulo EXtract PUrge and LOad data from a PostgreSQL instances to another one
package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/lib/pq"
	log "github.com/sirupsen/logrus"
)

// Config store the whole configuration read from json file
type Config struct {
	Tables []Table `json:"tables"`
}

// Columns contains a collection of Column
type Columns struct {
	Columns []Column `json:"columns"`
}

// Table represent a table with her property in configuration file
type Table struct {
	Name           string `json:"name"`
	FullName       string
	Columns        []Column `json:"columns"`
	Schema         string   `json:"schema"`
	CleanMethod    string   `json:"clean"`
	Filter         string   `json:"filter"`
	DeletionFilter string   `json:"deletion_filter"`
}

// Column in configuration
type Column struct {
	Name         string `json:"name"`
	Generator    string `json:"generator"`
	Min          int    `json:"min"`
	Max          int    `json:"max"`
	Timezone     string `json:"timezone"`
	SQLFunction  string `json:"function"`
	SequenceName string
	SeqLastValue int64
}

// Sequence with related attributes
type Sequence struct {
	TableName      string
	ColumnName     string
	SequenceName   string
	LastValue      int
	ColumnPosition int
	InitialValue   int64
	LastValueUsed  int64
}

// Table represent a table with her property in configuration file
type TriggerConstraint struct {
	TableFullName  string
	ConstraintName string
}

var (
	version    = "0.0.2"
	tryOnly    = false
	purgeOnly  = false
	configFile = "config.json"
)

func init() {
	// Set default LogLevel to INFO
	log.SetLevel(log.InfoLevel)

	// Check if stdout is connected to a terminal
	// If not remove colors in logs to be friendly
	if !IsTerminal(os.Stdout) {
		log.SetFormatter(&log.TextFormatter{
			DisableColors: true,
			FullTimestamp: true,
		})
	}
}

func main() {
	// Parse command line arguments
	flagParse()

	// Read the configuration
	config := readConfig(configFile)
	log.Debug("Read config done")
	log.Debug("Number of tables found in conf: ", len(config.Tables))

	// Read connection information from env variables
	conns := readEnv("SRC")
	connt := readEnv("DST")

	// Construct connection string
	conxSource, dsnSrc := getDsn(conns.Host, conns.Port, conns.User, conns.Pass, conns.Db, version)
	conxDestination, dsnDst := getDsn(connt.Host, connt.Port, connt.User, connt.Pass, connt.Db, version)

	// Connect to the database source
	log.Debug("Connect on source")
	dbSrc := connectDb(conxSource)
	log.Info(fmt.Sprintf("Use %s as source", dsnSrc))

	// Connect to the database destination
	log.Debug("Connect on destination")
	dbDst := connectDb(conxDestination)
	log.Info(fmt.Sprintf("Use %s as destination", dsnDst))

	// Extend the configuration with information at schema level in database
	sequencesArr, sequencesMap := GetSequencesInfo(dbDst)
	config = GetInfoFromDatabases(config, sequencesArr)

	// Start a transaction
	txDst, err := dbDst.Begin()
	if err != nil {
		log.Fatal("Error starting transaction: ", err)
	}

	// Build an Array with all table names in fullname form
	var tableList []string
	for _, t := range config.Tables {
		tableList = append(tableList, fullTableName(t.Schema, t.Name))
	}

	log.Debug("tableList contains : ", tableList)

	foreignKeys := make(map[string]string)

	// Read the foreign keys
	triggerConstraints := GetTriggerConstraints(dbDst, tableList, &foreignKeys)

	// Delete data on destination tables
	DeferForeignKeys(dbDst, triggerConstraints)
	purgeTarget(config, txDst)

	// if command line parameter set do purge and exit
	if purgeOnly {
		log.Debug("Exit on option, purge")
		CloseTx(txDst, tryOnly)
		ReactivateForeignKeys(dbDst, triggerConstraints)
		os.Exit(0)
	}

	// List all tables in insert order
	for _, t := range config.Tables {
		tableFullname := fullTableName(t.Schema, t.Name)
		log.Debug(fmt.Sprintf("Will insert in : %s", tableFullname))
	}

	// Loop over all tables configured
	for _, t := range config.Tables {
		tableFullname := fullTableName(t.Schema, t.Name)

		srcQuery := fmt.Sprintf("SELECT * FROM %s WHERE true", tableFullname)

		// Filter the data on source to fetch a subset of rows in a table
		if len(t.Filter) > 0 {
			srcQuery = fmt.Sprintf("%s AND %s", srcQuery, t.Filter)
		}
		startTime := time.Now()
		nbrows, _ := doTable(dbSrc, dbDst, txDst, t, srcQuery, &sequencesMap, foreignKeys)
		elapsedTime := time.Since(startTime)
		log.Info(fmt.Sprintf("%s : inserted %d rows total in %s", tableFullname, nbrows, elapsedTime))
	}

	if tryOnly {
		// Rollback the transaction on target as requested
		if err := txDst.Rollback(); err != nil {
			log.Fatal("Error committing transaction: ", err)
		}
		log.Info("Rollback on target")
	} else {
		// Restarting sequences
		resetAllSequences(dbDst, &sequencesMap)

		// Commit the transaction on target if all queries succeed
		if err := txDst.Commit(); err != nil {
			log.Fatal("Error committing transaction: ", err)
		} else {
			log.Info("Commit on target")
		}
	}
	log.Info("Thank you for using pg_expulo")
}

func doTable(dbSrc *sql.DB, dbDst *sql.DB, txDst *sql.Tx, t Table, srcQuery string, sequencesMap *map[string]Sequence, foreignKeys map[string]string) (int, string) {
	tableFullname := fullTableName(t.Schema, t.Name)

	log.Info(fmt.Sprintf("%s : read data fom table", tableFullname))

	rows, columns := queryTableSource(dbSrc, srcQuery)

	var multirows [][]interface{}
	lenColumns := len(columns)

	count := 0
	nbinsert := 0
	var errCode string
	var colnames []string
	var colparam []string

	initValues := make(map[string]int64)

	for _, ts := range *sequencesMap {
		code := fmt.Sprintf("%s.%s", ts.TableName, ts.ColumnName)
		// log.Debug(fmt.Sprintf("--- %s %d", code, ts.InitialValue))
		initValues[code] = ts.InitialValue
	}

	for rows.Next() {
		// Reset and increase value at first
		colnames = []string{}
		count++
		nbinsert++
		nbColumnModified := 1
		cols := make([]interface{}, lenColumns)
		columnPointers := make([]interface{}, len(cols))

		for i := range cols {
			columnPointers[i] = &cols[i]

		}
		rows.Scan(columnPointers...)

		colparam = []string{}
		var colValues []interface{}

		// Manage what we do it data here
		for i := range cols {
			cfvalue := "notfound"
			col, found := getCols(t, columns[i])
			if found {
				cfvalue = col.Generator
			} else {
				cfvalue = "notfound"
			}

			// If the configuration ignore the column it won't be present
			// in the INSERT statement

			colValues, colparam, nbColumnModified, colnames = FillColumn(t, col, cfvalue, colValues, colparam, nbColumnModified, cols, colnames, i, columns, sequencesMap, foreignKeys, initValues)

		}

		// INSERT
		multirows = append(multirows, colValues)

		batchSize := 1000
		if nbinsert > batchSize-1 {
			log.Debug(fmt.Sprintf("Insert %d rows in table %s", nbinsert, t.Name))
			nbinsert = 0
			_, errCode = insertMultiData(txDst, tableFullname, colnames, colparam, multirows)
			multirows = multirows[:0]
		}
	}
	if nbinsert > 0 {
		_, errCode = insertMultiData(txDst, tableFullname, colnames, colparam, multirows)
	}

	return count, errCode
}
