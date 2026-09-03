package main

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// Store 长期记忆库：先弄一个仓库
type Store struct {
	db *sql.DB
}

// NewStore 打开（不存在则创建）数据库文件并建表。给 db 赋值。
func NewStore(path string) (*Store, error) {
	// 这里没有真的打开文件，只是初始化
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS memories (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		content    TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Add 结束后写入记忆
func (s *Store) Add(content string) error {
	_, err := s.db.Exec("INSERT INTO memories (content) VALUES (?)", content)
	return err
}

// Search 开始前检索记忆：返回包含 keyword 的最近 limit 条
func (s *Store) Search(keyword string, limit int) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT content FROM memories WHERE content LIKE '%' || ? || '%' ORDER BY id DESC LIMIT ?",
		keyword, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Close 关闭数据库
func (s *Store) Close() error {
	return s.db.Close()
}
