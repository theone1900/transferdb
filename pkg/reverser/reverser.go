/*
Copyright © 2020 Marvin

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package reverser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"

	"github.com/wentaojin/transferdb/utils"

	"github.com/wentaojin/transferdb/service"

	"github.com/xxjwxc/gowp/workpool"
	"go.uber.org/zap"
)

func ReverseOracleToMySQLTable(engine *service.Engine, cfg *service.CfgFile) error {
	startTime := time.Now()
	service.Logger.Info("reverse table oracle to mysql start",
		zap.String("schema", cfg.SourceConfig.SchemaName))

	// 只提供表结构转换文本输出，不提供直写下游，故注释下游检查项
	//if err := reverseOracleToMySQLTableInspect(engine, cfg); err != nil {
	//	return err
	//}

	// 获取待转换表
	service.Logger.Info("get oracle to mysql all tables")

	exporterTableSlice, err := cfg.GenerateTables(engine)
	if err != nil {
		return err
	}

	tables, partitionTableList, err := GenerateOracleToMySQLTables(engine, exporterTableSlice, cfg.SourceConfig.SchemaName, cfg.TargetConfig.SchemaName, cfg.TargetConfig.Overwrite)
	if err != nil {
		return err
	}

	var (
		pwdDir                         string
		fileReverse, fileCompatibility *os.File
	)
	pwdDir, err = os.Getwd()
	if err != nil {
		return err
	}

	fileReverse, err = os.OpenFile(filepath.Join(pwdDir, "reverse.sql"), os.O_WRONLY|os.O_CREATE|os.O_APPEND|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer fileReverse.Close()

	fileCompatibility, err = os.OpenFile(filepath.Join(pwdDir, "compatibility.sql"), os.O_WRONLY|os.O_CREATE|os.O_APPEND|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer fileCompatibility.Close()

	service.Logger.Info("reverse", zap.String("create table and index output", filepath.Join(pwdDir, "reverse.sql")))
	service.Logger.Info("compatibility", zap.String("maybe exist compatibility output", filepath.Join(pwdDir, "compatibility.sql")))

	if len(partitionTableList) > 0 {
		var builder strings.Builder
		builder.WriteString("/*\n")
		builder.WriteString(fmt.Sprintf(" oracle partition table maybe mysql has compatibility, will convert to normal table, please manual adjust\n"))
		t := table.NewWriter()
		t.SetStyle(table.StyleLight)
		t.AppendHeader(table.Row{"SCHEMA", "ORACLE PARTITION LIST", "SUGGEST"})

		for _, part := range partitionTableList {
			t.AppendRows([]table.Row{
				{cfg.SourceConfig.SchemaName, part, "Manual Create And Adjust Table"},
			})
		}
		t.SetColumnConfigs([]table.ColumnConfig{
			{Number: 1, AutoMerge: true},
			{Number: 3, AutoMerge: true},
		})

		builder.WriteString(t.Render() + "\n")
		builder.WriteString("*/\n")
		if _, err = fileCompatibility.WriteString(builder.String()); err != nil {
			return err
		}
	}

	// 设置工作池
	// 设置 goroutine 数
	wrReverse := &FileMW{sync.Mutex{}, fileReverse}
	wrComp := &FileMW{sync.Mutex{}, fileCompatibility}

	wp := workpool.New(cfg.AppConfig.Threads)

	for _, table := range tables {
		// 变量替换，直接使用原变量会导致并发输出有问题
		tbl := table
		wrMR := wrReverse
		wrCMP := wrComp
		wp.Do(func() error {
			createSQL, compatibilitySQL, errMSg := tbl.GenerateAndExecMySQLCreateSQL()
			if errMSg != nil {
				return errMSg
			}
			if _, errMSg = fmt.Fprintln(wrMR, createSQL); errMSg != nil {
				return err
			}
			if _, errMSg = fmt.Fprintln(wrCMP, compatibilitySQL); errMSg != nil {
				return err
			}
			return nil
		})
	}
	if err = wp.Wait(); err != nil {
		return err
	}

	endTime := time.Now()
	if !wp.IsDone() {
		service.Logger.Error("reverse table oracle to mysql failed",
			zap.String("cost", endTime.Sub(startTime).String()),
			zap.Error(fmt.Errorf("reverse table task failed, please clear and rerunning")),
			zap.Error(err))
		return fmt.Errorf("reverse table task failed, please clear and rerunning, error: %v", err)
	}
	service.Logger.Info("reverse table oracle to mysql finished",
		zap.String("cost", endTime.Sub(startTime).String()))
	return nil
}

// 表转换前检查
func reverseOracleToMySQLTableInspect(engine *service.Engine, cfg *service.CfgFile) error {
	if err := engine.IsExistOracleSchema(cfg.SourceConfig.SchemaName); err != nil {
		return err
	}
	ok, err := engine.IsExistMySQLSchema(cfg.TargetConfig.SchemaName)
	if err != nil {
		return err
	}
	if !ok {
		_, _, err := service.Query(engine.MysqlDB, fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s`, cfg.TargetConfig.SchemaName))
		if err != nil {
			return err
		}
		return nil
	}

	// 获取 oracle 导出转换表列表
	var exporterTableSlice []string
	if len(cfg.SourceConfig.IncludeTable) != 0 {
		if err := engine.IsExistOracleTable(cfg.SourceConfig.SchemaName, cfg.SourceConfig.IncludeTable); err != nil {
			return err
		}
		exporterTableSlice = append(exporterTableSlice, cfg.SourceConfig.IncludeTable...)
	}

	if len(cfg.SourceConfig.ExcludeTable) != 0 {
		exporterTableSlice, err = engine.FilterDifferenceOracleTable(cfg.SourceConfig.SchemaName, cfg.SourceConfig.ExcludeTable)
		if err != nil {
			return err
		}
	}

	// 检查源端 schema 导出表是否存在目标端 schema 内
	existMysqlTables, err := engine.FilterIntersectionMySQLTable(cfg.TargetConfig.SchemaName, exporterTableSlice)
	if err != nil {
		return err
	}
	if len(existMysqlTables) > 0 {
		for _, tbl := range existMysqlTables {
			if cfg.TargetConfig.Overwrite {
				if err := engine.RenameMySQLTableName(cfg.TargetConfig.SchemaName, tbl); err != nil {
					return err
				}
			} else {
				// 表跳过重命名以及创建
				service.Logger.Warn("appear warning",
					zap.String("schema", cfg.TargetConfig.SchemaName),
					zap.String("table", tbl),
					zap.String("warn",
						fmt.Sprintf("config file params overwrite value false, table skip create")))
			}
		}
	}
	return nil
}

// 获取表列表
func GenerateOracleToMySQLTables(engine *service.Engine, exporterTableSlice []string, sourceSchema, targetSchema string, overwrite bool) ([]Table, []string, error) {
	// 筛选过滤分区表并打印警告
	partitionTables, err := engine.FilterOraclePartitionTable(sourceSchema, exporterTableSlice)
	if err != nil {
		return []Table{}, partitionTables, err
	}

	if len(partitionTables) != 0 {
		service.Logger.Warn("partition tables",
			zap.String("schema", sourceSchema),
			zap.String("partition table list", fmt.Sprintf("%v", partitionTables)),
			zap.String("suggest", "if necessary, please manually convert and process the tables in the above list"))
	}

	// 数据库查询获取自定义表结构转换规则
	var (
		tables []Table
		// 表名转换
		tableNameSlice []map[string]TableName
		// 表字段类型转换
		columnTypesMap map[string][]ColumnType
	)
	columnTypesMap = make(map[string][]ColumnType)

	// todo: 自定义表名适配删除 - 数据同步不支持表名不一致
	//customTableNameSlice, err := engine.GetCustomTableNameMap(cfg.SourceConfig.SchemaName)
	//if err != nil {
	//	return []Table{}, err
	//}

	for _, tbl := range exporterTableSlice {
		tableNameSlice = append(tableNameSlice, map[string]TableName{
			tbl: {
				SourceTableName: tbl,
				TargetTableName: tbl,
			},
		})
		// todo: 自定义表名适配删除 - 数据同步不支持表名不一致
		//if len(customTableNameSlice) != 0 {
		//	for _, tblName := range customTableNameSlice {
		//		if strings.ToUpper(tbl) == strings.ToUpper(tblName.SourceTableName) {
		//			if tblName.TargetTableName != "" {
		//				tableNameSlice = append(tableNameSlice, map[string]TableName{
		//					tbl: {
		//						SourceTableName: tbl,
		//						TargetTableName: tblName.TargetTableName,
		//					},
		//				})
		//			}
		//		}
		//	}
		//} else {
		//	tableNameSlice = append(tableNameSlice, map[string]TableName{
		//		tbl: {
		//			SourceTableName: tbl,
		//			TargetTableName: tbl,
		//		},
		//	})
		//}
	}

	customSchemaColumnTypeSlice, err := engine.GetCustomSchemaColumnTypeMap(sourceSchema)
	if err != nil {
		return []Table{}, partitionTables, err
	}
	customTableColumnTypeSlice, err := engine.GetCustomTableColumnTypeMap(sourceSchema)
	if err != nil {
		return []Table{}, partitionTables, err
	}

	// 加载字段类型转换规则
	// 字段类型转换规则判断，默认采用内置默认字段类型转换
	switch {
	case len(customSchemaColumnTypeSlice) != 0 && len(customTableColumnTypeSlice) == 0:
		for _, tbl := range exporterTableSlice {
			var colTypes []ColumnType
			for _, tblName := range customSchemaColumnTypeSlice {
				colTypes = append(colTypes, ColumnType{
					SourceColumnType: tblName.SourceColumnType,
					TargetColumnType: tblName.GetCustomSchemaColumnType(),
				})
			}
			columnTypesMap[tbl] = colTypes
		}
	case len(customSchemaColumnTypeSlice) == 0 && len(customTableColumnTypeSlice) != 0:
		for _, tbl := range exporterTableSlice {
			var colTypes []ColumnType
			for _, tblName := range customTableColumnTypeSlice {
				colTypes = append(colTypes, ColumnType{
					SourceColumnType: tblName.SourceColumnType,
					TargetColumnType: tblName.GetCustomTableColumnType(tbl),
				})
			}
			columnTypesMap[tbl] = colTypes
		}
	case len(customSchemaColumnTypeSlice) != 0 && len(customTableColumnTypeSlice) != 0:
		// 表字段类型优先级 > 库级别
		var customTableSlice []string
		// 获取所有任务表库级别字段类型
		for _, tbl := range exporterTableSlice {
			var colTypes []ColumnType
			for _, tblName := range customSchemaColumnTypeSlice {
				colTypes = append(colTypes, ColumnType{
					SourceColumnType: tblName.SourceColumnType,
					TargetColumnType: tblName.GetCustomSchemaColumnType(),
				})
			}
			columnTypesMap[tbl] = colTypes
		}

		// 加载获取自定义表字段类型转换规则
		// 处理情况:
		// - 自定义表字段类型规则不存在，而库字段类型存在的情况，则使用库字段类型转换规则
		// - 自定义表字段类型规则存在，而库字段类型也存在的情况，则使用表字段类型转换规则
		// - 两者都不存在，则不追加任何转换规则，字段类型转换时使用内置类型转换规则
		for _, tblName := range customTableColumnTypeSlice {
			if utils.IsContainString(exporterTableSlice, tblName.SourceTableName) {
				tmpColTypes := columnTypesMap[tblName.SourceTableName]
				for idx, col := range tmpColTypes {
					if strings.ToUpper(tblName.SourceColumnType) == strings.ToUpper(col.SourceColumnType) {
						columnTypesMap[tblName.SourceTableName][idx].TargetColumnType = tblName.GetCustomTableColumnType(tblName.SourceTableName)
					} else {
						columnTypesMap[tblName.SourceTableName] = append(columnTypesMap[tblName.SourceTableName], ColumnType{
							SourceColumnType: tblName.SourceColumnType,
							TargetColumnType: tblName.GetCustomTableColumnType(tblName.SourceTableName),
						})
					}
				}
				customTableSlice = append(customTableSlice, tblName.SourceTableName)
			}
		}

		// 筛选过滤不属于自定义表字段类型规则的表并加载获取转换规则
		notLayInCustomTableSlice := utils.FilterDifferenceStringItems(exporterTableSlice, customTableSlice)
		for _, tbl := range notLayInCustomTableSlice {
			var colTypes []ColumnType
			for _, tblName := range customSchemaColumnTypeSlice {
				colTypes = append(colTypes, ColumnType{
					SourceColumnType: tblName.SourceColumnType,
					TargetColumnType: tblName.GetCustomSchemaColumnType(),
				})
			}
			columnTypesMap[tbl] = colTypes
		}

	}

	// 返回需要转换 schema table
	for _, tbl := range exporterTableSlice {
		var table Table
		table.SourceSchemaName = sourceSchema
		table.TargetSchemaName = targetSchema
		// 表名规则
		for _, t := range tableNameSlice {
			if _, ok := t[tbl]; ok {
				table.SourceTableName = t[tbl].SourceTableName
				table.TargetTableName = t[tbl].TargetTableName
			} else {
				table.SourceTableName = tbl
			}
		}
		// 表字段类型规则
		if _, ok := columnTypesMap[tbl]; ok {
			table.ColumnTypes = columnTypesMap[tbl]
		}
		table.Engine = engine
		table.Overwrite = overwrite
		tables = append(tables, table)
	}
	return tables, partitionTables, nil
}
