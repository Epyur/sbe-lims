package main

import (
	"context"
	"log"
	"time"
)

// rolloutRequests — одноразовая миграция данных (дизайн 2026-08-18): каждая
// заявка с N методами (старая модель request_methods) раскатывается на N
// под-заявок, где method_id/customer_number/lab_number лежат прямо в requests.
//
// Правила:
//   - общий NNN у группы = одинаковые number_seq + number_year (без шапки);
//   - files копируются во все под-заявки группы (объекты S3 не дублируются);
//   - measurement_results / aggregated_results переносятся по (request_id, method_id)
//     в под-заявку с этим методом;
//   - исходная мульти-методная заявка удаляется (каскадом уходят старые связки);
//   - request_seq не трогается (NNN не переиспользуется);
//   - заявки с ровно 1 методом просто получают метод/номера в свою строку.
//
// Идемпотентность: процесс детектится по наличию строк в request_methods;
// после раскатки таблица пуста и повторный запуск ничего не делает.
func (s *Server) rolloutRequests(ctx context.Context) error {
	var legacy int64
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM request_methods`).Scan(&legacy); err != nil {
		return err
	}
	if legacy == 0 {
		log.Printf("rollout requests: legacy request_methods not found, skip")
		return nil
	}

	type rm struct {
		requestID   int64
		methodID    int64
		customer    string
		lab         string
		seq         int64
		year        int
		title       string
		descr       string
		objectID    *int64
		projID      *int64
		groupID     *int64
		owner       string
		status      string
		priority    string
		testPurpose string
		extLAbID    int64
		ekn         string
		createdAt   time.Time
		updatedAt   time.Time
	}

	rows, err := s.pool.Query(ctx, `
SELECT rm.request_id, rm.method_id, rm.customer_number, rm.lab_number,
	r.number_seq, r.number_year, r.title, r.description,
	r.object_id, r.project_id, r.group_id, r.owner_email, r.status,
	r.priority, r.test_purpose, r.external_lab_id, r.ekn,
	r.created_at, r.updated_at
FROM request_methods rm
JOIN requests r ON r.id = rm.request_id
ORDER BY r.id, rm.method_id`)
	if err != nil {
		return err
	}

	// Группируем методы по request_id, сохраняя порядок (по method_id).
	grouped := map[int64][]rm{}
	order := make([]int64, 0)
	for rows.Next() {
		var m rm
		if err := rows.Scan(&m.requestID, &m.methodID, &m.customer, &m.lab,
			&m.seq, &m.year, &m.title, &m.descr,
			&m.objectID, &m.projID, &m.groupID, &m.owner, &m.status,
			&m.priority, &m.testPurpose, &m.extLAbID, &m.ekn,
			&m.createdAt, &m.updatedAt); err != nil {
			rows.Close()
			return err
		}
		if _, ok := grouped[m.requestID]; !ok {
			order = append(order, m.requestID)
		}
		grouped[m.requestID] = append(grouped[m.requestID], m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	rolled := 0
	for _, rid := range order {
		ms := grouped[rid]

		if len(ms) == 1 {
			// Один метод — переносим прямо в строку заявки.
			m := ms[0]
			if _, err := tx.Exec(ctx, `
UPDATE requests SET method_id = $2, customer_number = $3, lab_number = $4, updated_at = now()
WHERE id = $1`, rid, m.methodID, m.customer, m.lab); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`DELETE FROM request_methods WHERE request_id = $1`, rid); err != nil {
				return err
			}
			rolled++
			continue
		}

		// N > 1: создаём N под-заявок.
		subIDs := make([]int64, 0, len(ms))
		for _, m := range ms {
			var subID int64
			if err := tx.QueryRow(ctx, `
INSERT INTO requests (number_seq, number_year, title, description, object_id, project_id,
	group_id, owner_email, status, priority, test_purpose, external_lab_id, ekn,
	method_id, customer_number, lab_number, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
RETURNING id`,
				m.seq, m.year, m.title, m.descr, m.objectID, m.projID,
				m.groupID, m.owner, m.status, m.priority, m.testPurpose, m.extLAbID, m.ekn,
				m.methodID, m.customer, m.lab, m.createdAt, m.updatedAt).Scan(&subID); err != nil {
				return err
			}
			subIDs = append(subIDs, subID)

			// Файлы копируются во все под-заявки группы.
			if _, err := tx.Exec(ctx, `
INSERT INTO files (request_id, file_key, file_name, file_size, file_url, uploaded_by, created_at)
SELECT $1, file_key, file_name, file_size, file_url, uploaded_by, created_at
FROM files WHERE request_id = $2`, subID, rid); err != nil {
				return err
			}

			// Результаты ЛИМС — в под-заявку с этим методом.
			if _, err := tx.Exec(ctx, `
UPDATE measurement_results SET request_id = $1 WHERE request_id = $2 AND method_id = $3`,
				subID, rid, m.methodID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
UPDATE aggregated_results SET request_id = $1 WHERE request_id = $2 AND method_id = $3`,
				subID, rid, m.methodID); err != nil {
				return err
			}
		}

		// Исходная заявка и старые связки удаляются (каскад).
		if _, err := tx.Exec(ctx, `DELETE FROM requests WHERE id = $1`, rid); err != nil {
			return err
		}
		rolled++
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	log.Printf("rollout requests: %d legacy requests migrated (multi-method -> per-method)", rolled)
	return nil
}
