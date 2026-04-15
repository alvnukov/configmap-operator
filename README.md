# configmap-operator

`configmap-operator` синхронизирует `ConfigMap` и монтирования в `Deployment` по декларативному YAML-конфигу.

Оператор:
- следит за `Deployment` (watch + reconcile очередь);
- создает/обновляет нужные `ConfigMap`;
- патчит `PodTemplate` целевых `Deployment`, добавляя volumes/volumeMounts;
- инициирует rollout только когда меняются данные `ConfigMap`.

## Содержание

- [Что делает оператор](#что-делает-оператор)
- [Архитектура reconcile](#архитектура-reconcile)
- [Требования](#требования)
- [CLI параметры](#cli-параметры)
- [Быстрый старт](#быстрый-старт)
- [Конфигурация](#конфигурация)
- [Запуск без Helm](#запуск-без-helm)
- [Запуск через Helm chart](#запуск-через-helm-chart)
- [Локальная разработка](#локальная-разработка)
- [Тестирование](#тестирование)
- [CI/CD](#cicd)
- [Безопасность и supply chain](#безопасность-и-supply-chain)
- [Наблюдаемость и диагностика](#наблюдаемость-и-диагностика)
- [Ограничения и edge cases](#ограничения-и-edge-cases)

## Что делает оператор

1. Читает YAML-конфиг с описанием `deployments -> containers -> configMaps -> data`.
2. Валидирует конфиг в strict-режиме (неизвестные поля запрещены).
3. На старте синхронизирует `ConfigMap` из конфига.
4. Запускает informer на `Deployment` и обрабатывает add/update события.
5. Для каждого целевого `Deployment`:
- обеспечивает наличие/актуальность `ConfigMap`;
- добавляет `volume` с `configMap.items`;
- добавляет `volumeMount` по `mountPath`/`subPath`;
- обновляет аннотацию хеша конфига `configmap-operator.io/config-hash`;
- при изменении данных `ConfigMap` обновляет `configmap-operator.io/restarted-at` (триггер rollout).

## Архитектура reconcile

Ключевые параметры контроллера:
- `resyncPeriod`: `30s`
- `workerCount`: `2`
- `requestTimeout`: `10s`
- `maxReconcileRetries`: `10`

Поведение очереди:
- используется rate-limiting workqueue;
- при конфликте/временной ошибке выполняется retry;
- после `maxReconcileRetries` ключ дропается с ошибкой в лог.

Идемпотентность:
- повторный reconcile без изменений не патчит ресурсы;
- `restarted-at` не меняется, если данные `ConfigMap` уже совпадают;
- лишние ключи в `ConfigMap` удаляются (источник истины — конфиг).

## Требования

- Go: `1.26.2` (или `1.26.x` для локальной разработки)
- Kubernetes API: `apps/v1` (`Deployment`), `core/v1` (`ConfigMap`)
- Доступ к cluster API из pod или через kubeconfig
- Для Helm-пути: `helm`
- Для локального e2e: `docker`, `kubectl`, `k3d`

Минимальные RBAC права для оператора:
- `configmaps`: `get,list,watch,create,update,patch`
- `deployments`: `get,list,watch,update,patch`

## CLI параметры

- `-config` (default: `config.yaml`) — путь к YAML-конфигу оператора
- `-kubeconfig` (default: `~/.kube/config`) — путь к kubeconfig для запуска вне кластера

Логика выбора Kubernetes-конфига:
- сначала используется `-kubeconfig`;
- если не удалось, оператор пробует `InClusterConfig`;
- если оба варианта недоступны, процесс завершается с ошибкой.

## Быстрый старт

### 1) Подготовьте конфиг

Пример минимального рабочего конфига:

```yaml
deployments:
  - namespace: default
    name: my-app
    containers:
      - name: app
        configMaps:
          - name: my-app-config
            data:
              - key: app.conf
                value: |
                  mode=prod
                mountPath: /etc/my-app/app.conf
                subPath: app.conf
```

### 2) Запустите оператор

```bash
go run . -config ./config.yaml
```

Если запуск вне кластера:

```bash
go run . -kubeconfig ~/.kube/config -config ./config.yaml
```

## Конфигурация

Файл конфигурации описывается структурой:

```yaml
deployments:
  - namespace: <string>
    name: <string>
    containers:
      - name: <string>
        configMaps:
          - name: <string>
            data:
              - key: <string>
                value: <string>
                mountPath: <absolute path in container>
                subPath: <optional file name in ConfigMap volume>
```

### Валидация (обязательные и строгие правила)

Оператор отклоняет конфиг, если:
- есть неизвестные поля;
- `deployments` пустой;
- пустые `namespace`/`name` у deployment;
- дублируются deployment по `namespace/name`;
- пустое имя container;
- дублируются container внутри deployment;
- пустое имя configMap;
- дублируются configMap в одном container;
- пустой `key` или `mountPath` у data-элемента;
- дублируется `key` внутри одного configMap;
- дублируется mount target (`mountPath + subPath`) в одном configMap;
- один и тот же mount target используется разными configMap в одном container;
- один и тот же `configMap.name` в разных container одного deployment имеет конфликтующие `data`.

### Семантика полей

- `subPath`:
  - если задан, монтируется конкретный файл из `ConfigMap`;
  - если не задан, для `configMap.items.path` используется значение `key`.
- `value`:
  - используется как содержимое соответствующего ключа в `ConfigMap`.

## Запуск без Helm

### Локально

```bash
go test ./...
go build -o bin/configmap-operator .
./bin/configmap-operator -kubeconfig ~/.kube/config -config ./config.yaml
```

### В контейнере

```bash
docker build -t ghcr.io/<owner>/configmap-operator:dev .
```

## Запуск через Helm chart

В репозитории есть чарт в директории `.helm` (на базе `helm-apps`).

### Базовая установка

```bash
helm dependency build .helm
helm lint .helm
helm upgrade --install configmap-operator .helm \
  -n configmap-operator \
  --create-namespace
```

### Обязательные overrides

Нужно переопределить:
- образ оператора:
  - `apps-stateless.configmap-operator.containers.main.image.repository`
  - `apps-stateless.configmap-operator.containers.main.image.name`
  - `apps-stateless.configmap-operator.containers.main.image.staticTag`
- содержимое операторского конфига:
  - `apps-stateless.configmap-operator.containers.main.configFiles.config.yaml.content`

Пример:

```bash
helm upgrade --install configmap-operator .helm \
  -n configmap-operator \
  --create-namespace \
  --set-string apps-stateless.configmap-operator.containers.main.image.repository=ghcr.io/my-org \
  --set-string apps-stateless.configmap-operator.containers.main.image.name=configmap-operator \
  --set-string apps-stateless.configmap-operator.containers.main.image.staticTag=v1.0.0 \
  --set-file apps-stateless.configmap-operator.containers.main.configFiles.config.yaml.content=./config.yaml
```

## Локальная разработка

Основные команды:

```bash
go test ./...
go vet ./...
gofmt -w .
```

Сборка:

```bash
go build -o bin/configmap-operator .
```

## Тестирование

### Unit/logic

```bash
go test ./...
```

Покрываются:
- strict parse/validation;
- reconcile `ConfigMap`;
- reconcile `Deployment`;
- идемпотентность и hash-аннотации.

### Helm

```bash
helm dependency build .helm
helm lint .helm
helm template configmap-operator .helm --namespace e2e > /tmp/rendered.yaml
```

### E2E (k3s/k3d)

Сценарий: `scripts/e2e-k3s.sh`

Что проверяет e2e:
- установка чарта;
- создание/обновление `ConfigMap` `e2e-config`;
- фактическое монтирование файла `/etc/config/app.conf` в workload pod.

Переменные окружения сценария:
- `E2E_NAMESPACE` (default: `e2e`)
- `E2E_RELEASE_NAME` (default: `configmap-operator`)
- `E2E_IMAGE` (default: `ghcr.io/example/configmap-operator:e2e`)
- `E2E_EXPECTED_VALUE` (default: `version=1`)

Пример локального прогона:

```bash
k3d cluster create ci --agents 1 --wait

docker build -t ghcr.io/example/configmap-operator:e2e .
k3d image import ghcr.io/example/configmap-operator:e2e -c ci

./scripts/e2e-k3s.sh
```

## CI/CD

### CI (`.github/workflows/ci.yml`)

Запускается на `pull_request` и `push` в `main`.

Jobs:
- `go-tests`
  - `gofmt` check
  - `go vet`
  - `go test ./...`
- `security-audit`
  - `govulncheck`
  - `gosec` (SARIF upload + enforcement)
  - `trivy fs` (HIGH/CRITICAL, блокирующий)
- `helm-chart`
  - dependency build
  - lint
  - template
  - `kubeconform` strict validation
- `e2e-k3s`
  - поднимает k3s (k3d)
  - собирает образ
  - импортирует образ в кластер
  - запускает `scripts/e2e-k3s.sh`

### Publish (`.github/workflows/publish-image.yml`)

Публикация образа запускается:
- после успешного `CI` на `main` (`workflow_run`);
- при push тега `v*`;
- вручную (`workflow_dispatch`).

Что делает:
- buildx multi-arch push в `ghcr.io/<owner>/<repo>`;
- платформы: `linux/amd64`, `linux/arm64`, `linux/arm/v7`;
- генерирует `sbom` и `provenance`;
- подписывает образ keyless `cosign`;
- выполняет `trivy` скан опубликованного digest.

## Безопасность и supply chain

- Runtime image: `distroless` (`gcr.io/distroless/static-debian12:nonroot`)
- Бинарь собирается с `CGO_ENABLED=0`, `-trimpath`, stripped symbols
- В CI включены:
  - dependency vuln scan (`govulncheck`)
  - static analysis (`gosec`)
  - filesystem/image scanning (`trivy`)
  - SARIF публикация в GitHub Security

## Наблюдаемость и диагностика

Логирование:
- `zap` production logger.

Полезные команды:

```bash
kubectl -n <ns> logs deploy/configmap-operator --tail=200
kubectl -n <ns> get configmap <name> -o yaml
kubectl -n <ns> get deploy <name> -o yaml
```

Что смотреть в `Deployment` после reconcile:
- `spec.template.metadata.annotations["configmap-operator.io/config-hash"]`
- `spec.template.metadata.annotations["configmap-operator.io/restarted-at"]`
- `spec.template.spec.volumes[*].configMap`
- `spec.template.spec.containers[*].volumeMounts`

Что смотреть в `ConfigMap`:
- `metadata.labels["app.kubernetes.io/managed-by"] == "configmap-operator"`
- `data` точно соответствует конфигу.

## Ограничения и edge cases

- Оператор работает только с объектами, перечисленными в конфиге.
- Если целевой container отсутствует в deployment, оператор пишет warning и пропускает этот container-конфиг.
- Оператор не удаляет deployment/volumeMount/volume, которые не описаны в конфиге, но обновляет совпадающие по имени `ConfigMap` объекты.
- При конфликтных обновлениях используется retry на `Update` (optimistic concurrency).
- Некорректный конфиг блокирует старт оператора на этапе инициализации.
