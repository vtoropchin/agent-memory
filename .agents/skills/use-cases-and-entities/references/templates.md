# Шаблоны: варианты использования, сущности, порты

Псевдокод намеренно нейтрален к языку. Переведите конструкции в идиомы своего стека.

---

## Шаблон описания варианта использования

```
Вариант использования: Оформить кредит
Актор: сотрудник кредитного отдела
Ввод:
  - идентификатор клиента
  - сумма
  - срок
Предусловия (правила приложения):
  - контактные данные собраны и проверены
  - кредитный балл клиента >= 500
Шаги:
  1. Загрузить клиента через порт Customers
  2. Создать сущность Loan (критические правила: расчёт процентов, график платежей)
  3. Проверить инвариант: сумма не превышает лимит клиента  [критическое правило]
  4. Сохранить через порт Loans
  5. Передать результат в порт вывода
Вывод:
  - номер кредита, график платежей, итоговая сумма
Ошибки:
  - клиент не найден, балл ниже порога, лимит превышен
```

Ни слова о HTTP, экранах, таблицах. Если такие слова появляются — они относятся к адаптеру.

---

## Сущность

```
module Loan:
    data:
        principal, rate, schedule, state      // критические бизнес-данные

    // критические бизнес-правила — верны и без компьютера
    function interestFor(period) -> Money
    function applyPayment(amount) -> Result
    function isDelinquent(asOf: Date) -> bool
```

Запрещено внутри: `save`, `load`, SQL, HTTP, аннотации ORM/DI/сериализации, обращение к
системным часам напрямую (время передаётся аргументом или через порт `Clock`).

---

## Интерактор варианта использования

```
module OpenLoanInteractor implements OpenLoanInputPort:

    dependencies (порты, объявленные в ядре):
        customers: Customers
        loans: Loans
        clock: Clock
        output: OpenLoanOutputPort

    function execute(input: OpenLoanInput):
        customer = customers.byId(input.customerId)
            or return output.presentNotFound(input.customerId)

        if customer.creditScore < 500:
            return output.presentRejected(reason = LOW_SCORE)

        loan = Loan.open(input.amount, input.termMonths, clock.today())
        loans.add(loan)

        output.present(OpenLoanOutput{
            loanId: loan.id,
            schedule: loan.schedule.asPlainRows(),   // простые данные, не сущности
            total: loan.totalCost()
        })
```

Ключевые свойства: никаких типов инфраструктуры, никакого форматирования, никакой навигации,
одна операция, все внешние эффекты — через порты.

---

## Модели ввода и вывода

```
struct OpenLoanInput:
    customerId: string
    amount: decimal
    termMonths: int

struct OpenLoanOutput:
    loanId: string
    schedule: list of { dueDate: Date, amount: decimal }
    total: decimal
```

Правила: без методов с бизнес-логикой, без ссылок на сущности, без типов фреймворка,
без наследования от классов транспорта.

---

## Порты

```
interface Customers:                        // хранилище, язык предметной области
    byId(id) -> Customer?
    withOverdueLoans(asOf: Date) -> list<Customer>

interface Loans:
    add(loan: Loan)
    byCustomer(id) -> list<Loan>

interface Clock:                            // время — тоже внешний мир
    today() -> Date

interface NotifyCustomer:
    loanApproved(customerId, loanId)

interface OpenLoanOutputPort:               // порт вывода
    present(output: OpenLoanOutput)
    presentRejected(reason)
    presentNotFound(customerId)
```

Порты объявлены **в ядре**, реализованы снаружи, связаны в `Main`.

---

## Адаптеры

```
// Контроллер: транспорт → модель ввода
module HttpOpenLoanController:
    function handle(httpRequest):
        input = OpenLoanInput{
            customerId: httpRequest.body["customer_id"],
            amount:     parseDecimal(httpRequest.body["amount"]),
            termMonths: parseInt(httpRequest.body["term"])
        }
        interactor.execute(input)          // ответ уйдёт через презентатор
```

```
// Презентатор: модель вывода → модель представления (всё уже отформатировано)
module HttpOpenLoanPresenter implements OpenLoanOutputPort:
    function present(output):
        viewModel.loanId = output.loanId
        viewModel.total  = formatMoney(output.total, "RUB")
        viewModel.rows   = output.schedule.map(r => {
                              date:   formatDate(r.dueDate, "dd.MM.yyyy"),
                              amount: formatMoney(r.amount, "RUB")
                          })
        viewModel.showWarning = output.total > threshold
```

```
// Шлюз хранилища: реализация порта, «скромный объект»
module SqlLoans implements Loans:
    function add(loan):
        row = mapToRow(loan)               // явный маппинг, не общий тип
        db.execute("insert into loans ...", row)
```

---

## Composition root (`Main`)

```
module Main:
    function start(config):
        db        = openDatabase(config.dsn)
        loans     = SqlLoans(db)
        customers = SqlCustomers(db)
        clock     = SystemClock()

        presenter  = HttpOpenLoanPresenter()
        interactor = OpenLoanInteractor(customers, loans, clock, presenter)
        controller = HttpOpenLoanController(interactor)

        httpServer.route("POST /loans", controller.handle)
        httpServer.listen(config.port)
```

`Main` — единственное место, где встречаются конкретные реализации. Вариантов `Main` может быть
несколько: dev, test, prod, по клиенту, по стране, по конфигурации.

---

## Структура каталогов, которая «кричит»

```
loans/                         ← предметная область, а не «controllers/»
    entities/
        loan
        customer
    usecases/
        open_loan/
            interactor
            input
            output
            ports
        close_loan/
        list_overdue_loans/
adapters/
    http/
    persistence/
    notifications/
main/
    dev
    prod
```

Проверка: по дереву каталогов виден список того, что система умеет делать, и не видно, чем
она это делает.

---

## Частые ошибки при заполнении шаблонов

| Ошибка | Почему плохо | Как правильно |
|---|---|---|
| `OpenLoanInput` наследует HTTP-модель | ядро зависит от транспорта | отдельная структура + маппинг в контроллере |
| Интерактор возвращает сущность наружу | внешний код завязывается на внутреннюю модель | возвращать структуру вывода |
| Порт `Loans.executeQuery(sql)` | SQL просочился в ядро | операции в терминах приложения |
| Сущность с `save()` | сущность знает о хранилище | сохранение — через порт из варианта использования |
| Один «UserInteractor» на 12 операций | разные акторы и причины изменения | один вариант использования — один модуль |
| Форматирование даты в интеракторе | презентационная деталь в ядре | форматирование в презентаторе |
| Прямой вызов `now()` в правиле | недетерминированный тест, скрытый I/O | порт `Clock` |
