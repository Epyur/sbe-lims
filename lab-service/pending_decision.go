package main

import "time"

// pendingResultMaxAge - сколько ждать заявку с таким external_id, прежде чем
// убрать результат из живой очереди. Счётчик попыток здесь не ограничитель:
// опрос идёт каждые ~2 минуты, и по нему запись выбывала бы через час.
const pendingResultMaxAge = 30 * 24 * time.Hour

type pendingAction int

const (
	pendingKeep pendingAction = iota
	pendingArchive
)

// pendingRowDecision - чистое решение о судьбе ожидающей записи, когда заявки
// с её external_id ещё нет. Решает возраст: счётчик попыток растёт каждые
// ~2 минуты, поэтому сам по себе он ничего не говорит о том, чего мы ждём.
func pendingRowDecision(age time.Duration) pendingAction {
	if age >= pendingResultMaxAge {
		return pendingArchive
	}
	return pendingKeep
}