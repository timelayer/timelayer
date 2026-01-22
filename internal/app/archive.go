package app

import (
	"compress/gzip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func forgetAndArchive(cfg Config, db any) error {
	sqlDB := db.(*sql.DB)

	entries, err := os.ReadDir(cfg.LogDir)
	if err != nil {
		return err
	}

	cutoff := time.Now().In(cfg.Location).AddDate(0, 0, -cfg.KeepRawDays)

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}

		date := strings.TrimSuffix(name, ".jsonl")
		d, err := time.ParseInLocation("2006-01-02", date, cfg.Location)
		if err != nil || !d.Before(cutoff) {
			continue
		}

		// 确保 daily summary 已存在
		if ok, _ := summaryExists(sqlDB, "daily", date); !ok {
			continue
		}

		srcPath := filepath.Join(cfg.LogDir, name)
		if err := appendToMonthlyArchive(cfg, date, srcPath); err != nil {
			continue
		}
		_ = os.Remove(srcPath)
	}

	return nil
}

func appendToMonthlyArchive(cfg Config, date, srcPath string) error {
	d, err := time.ParseInLocation("2006-01-02", date, cfg.Location)
	if err != nil {
		return err
	}

	monthKey := d.Format("2006-01")
	dstPath := filepath.Join(cfg.ArchiveDir, monthKey+".jsonl.gz")
	_ = os.MkdirAll(cfg.ArchiveDir, 0755)

	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	gw := gzip.NewWriter(out)
	defer gw.Close()

	_, err = io.Copy(gw, in)
	return err
}
