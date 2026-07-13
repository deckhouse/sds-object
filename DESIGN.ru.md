# sds-object — дизайн модуля

> Статус: дизайн (черновик). Этот документ описывает исходную целевую
> архитектуру и контракт CRD.
>
> **Обновление модели бакетов (актуальная реализация, COSI-выравнивание).**
> Модель приведена к COSI (`Bucket` + `BucketClaim`), CRD `ObjectStorageCluster`
> переименован в `ObjectStore`. Текущий набор из пяти ресурсов:
> - `ObjectStore` (**cluster-scoped**) — разворачивает data plane хранилища
>   (вне модели COSI); ранее назывался `ObjectStorageCluster`;
> - `Bucket` (**cluster-scoped**) — бакет-бэкенд: либо объявлен администратором
>   (Shared), либо создан контроллером под greenfield-`BucketClaim` (origin в
>   лейбле `storage.deckhouse.io/bucket-origin`);
> - `BucketClaim` (**namespaced**) — заявка на бакет: greenfield (создаёт
>   собственный приватный `Bucket`) или brownfield (`spec.existingBucketName` —
>   привязка Shared-бакета, гейтится `BucketClaimPolicy`, deny-by-default);
> - `BucketAccess` (namespaced) — запрос учётных данных к `BucketClaim` в том же
>   namespace: контроллер выдаёт отдельную пару ключей, пишет `Secret`
>   (owned by access) и поддерживает ротацию по аннотации
>   `storage.deckhouse.io/rotate`;
> - `BucketClaimPolicy` (cluster-scoped) — из каких namespace разрешена привязка
>   Shared-бакета через brownfield-`BucketClaim` (deny-by-default; имена + RE2).
>
> Также: у `ObjectStore` есть `spec.reclaimPolicy` (`Retain`/`Delete`, по
> умолчанию `Retain`); SeaweedFS использует общий PostgreSQL только в
> `High`, иначе встроенный leveldb. Разделы §3–§4 ниже описывают
> исходные имена полей (`clusterRef`, `bucketRef`); **актуальный контракт полей
> и имён — в `crds/` и `docs/`** (`objectStoreRef`, `bucketClaimName` и т. д.).

## 1. Назначение

`sds-object` управляет **S3-совместимым объектным хранилищем** в кластере
Deckhouse. Платформа noops: пользователь декларирует *что* ему нужно, модуль
сам разворачивает и сопровождает бэкенд. Низкоуровневые ручки бэкендов наружу
не выносятся — по аналогии с тем, как `sds-elastic` прячет настройки Ceph за
интент-абстракциями (`replication: ConsistencyAndAvailability` и т. п.).

Модуль предоставляет **пять CRD** (COSI-выравнивание):

| CRD | Scope | Назначение |
|-----|-------|------------|
| `ObjectStore` (ostore) | Cluster | Развернуть объектное хранилище одного из 4 типов (data plane; вне COSI) |
| `Bucket` (bkt) | Cluster | Бакет-бэкенд: Shared (объявлен админом) или созданный под `BucketClaim` |
| `BucketClaim` (bc) | **Namespaced** | Заявка на бакет: greenfield (свой приватный бакет) или brownfield (привязка Shared-бакета, гейтится политикой) |
| `BucketAccess` (ba) | **Namespaced** | Запросить учётные данные к `BucketClaim` — отдельная пара ключей + `Secret` рядом с приложением; ротация по аннотации |
| `BucketClaimPolicy` (bp) | Cluster | Задать, из каких namespace разрешена привязка Shared-бакета через brownfield-заявку (deny-by-default; имена + RE2-паттерны) |

Greenfield-заявка создаёт собственный приватный бакет (namespace-локальный, имя
под зарезервированным префиксом — не может совпасть с Shared-бакетом). Shared-бакет
— общая (cluster-scoped) платформенная сущность; привязать его из namespace можно
только brownfield-заявкой, гейтящейся `BucketClaimPolicy` (deny-by-default). Это
закрывает риск «захвата» чужого/системного бакета, который был возможен в исходной
namespaced-`ObjectBucket`-модели (см. историческую врезку выше и ADR).

Существующий placeholder `ObjectStorageClass` (cluster-scoped) при реализации
удалён и заменён на CRD выше (обновлены хуки удаления финализаторов,
вебхук-конфигурация и RBAC — см. §8).

## 2. Четыре типа кластера

Один CRD, поле `spec.type` (enum, immutable). Бэкенд скрыт за интент-именем.

| `spec.type` | Бэкенд | Размещение / данные | Сценарий |
|-------------|--------|---------------------|----------|
| `System` | Garage | StatefulSet (фикс. 3 реплики) на control-plane, node-sticky local PV | Системные нужды платформы (backup, registry, loki, …). Минимум зависимостей, не требует внешнего стораджа. |
| `Lightweight` | Garage | StatefulSet, PVC на StorageClass | Лёгкое хранилище для прикладных нужд небольшого объёма. |
| `Full` | SeaweedFS | StatefulSet (master/volume/filer+s3), PVC | Полноценное масштабируемое хранилище с репликацией/EC. |
| `Heavy` | sds-elastic (Ceph RGW) | RADOS Gateway поверх существующего CephCluster | Тяжёлое хранилище, переиспользует ёмкость и отказоустойчивость Ceph. |

Почему Garage для System и Lightweight: лёгкий S3-сервер на Rust, минимум
компонентов, умеет hostPath/локальные диски, простой admin API для управления
бакетами и ключами. Идеален для «коробочного» системного стораджа на мастерах.

Почему SeaweedFS для Full: распределённое хранилище с master/volume/filer,
S3-gateway, репликацией и erasure coding — масштабируется заметно лучше Garage.

Почему Ceph RGW для Heavy: при наличии `sds-elastic` (Rook Ceph) объектное
хранилище — это `CephObjectStore` поверх уже развёрнутого кластера. Никакого
отдельного дата-плейна, ёмкость и HA берутся у Ceph.

## 3. CRD `ObjectStore` (cluster-scoped)

### 3.1 Spec

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: ObjectStore
metadata:
  name: shared            # DNS-1123 label, <= 30 символов (префикс производных ресурсов)
spec:
  type: Lightweight       # System | Lightweight | Full | Heavy — REQUIRED, immutable

  # Ёмкость и сторадж. Игнорируется для type=Heavy (ёмкость у Ceph).
  # Для type=System поля storage.sizePerNode и redundancy задавать НЕЛЬЗЯ
  # (запрещено CEL): ёмкость — local PV, фактор закреплён на 3.
  storage:
    sizePerNode: 100Gi    # ёмкость на один узел data plane
    nodes: 3              # число узлов data plane (optional; иначе из redundancy)
    class: localpath      # имя StorageClass для PVC; REQUIRED для Lightweight/Full,
                          # игнорируется для System (local PV) и Heavy (Ceph)

  # Размещение dataplane. Для System перекрывается фиксированным
  # размещением на control-plane (см. ниже), nodeSelector можно не задавать.
  placement:
    nodeSelector: {}
    tolerations: []

  # Интент отказоустойчивости. Маппится в конкретные настройки бэкенда.
  # По умолчанию Standard. Нельзя задавать для type=System.
  redundancy: Standard    # None | Standard | High   (optional)

  # Только для type=Heavy: ссылка на ElasticCluster (sds-elastic),
  # поверх которого поднимается CephObjectStore.
  elasticClusterRef: ""   # REQUIRED iff type==Heavy, immutable
```

Минимальные примеры по типам:

```yaml
# 1. Системный кластер (garage на мастерах, node-sticky local PV)
spec:
  type: System            # всё остальное — дефолты
---
# 2. Lightweight (garage + PVC)
spec:
  type: Lightweight
  storage: { sizePerNode: 50Gi, class: localpath }
---
# 3. Full (seaweedfs)
spec:
  type: Full
  storage: { sizePerNode: 2Ti, class: replicated }
  redundancy: High
---
# 4. Heavy (Ceph RGW поверх sds-elastic)
spec:
  type: Heavy
  elasticClusterRef: main
```

### 3.2 Семантика `redundancy`

Интент отказоустойчивости → настройки бэкенда:

| Значение | Garage (`replication_factor`) | SeaweedFS (код репликации) | Ceph RGW (data-пул) |
|----------|-------------------------------|----------------------------|---------------------|
| `None` | 1 | `000` | `size=2`\* |
| `Standard` (default) | 3 | `001` | `size=3` |
| `High` | 5 | `002` | `size=4` |

\* Ceph `size=1` небезопасен, поэтому для `None` используется `size=2` с
отключённым `requireSafeReplicaSize`. Metadata-пул Ceph всегда `size=3`.

#### Реплики data plane и фактор репликации

Число реплик и итоговый фактор зависят от профиля, `redundancy` и (для профилей
на PVC) `spec.storage.nodes`. `spec.storage.sizePerNode` — ёмкость **на узел**.

| Профиль (движок) | Реплики data plane | Фактор репликации / аналог |
|------------------|--------------------|----------------------------|
| `System` (Garage) | фиксированные 3 реплики (StatefulSet, node-sticky local PV), независимо от числа мастеров | `3`, **pinned**; `redundancy`/`sizePerNode` задавать нельзя |
| `Lightweight` (Garage) | `spec.storage.nodes`, иначе из `redundancy`: None→1, Standard→3, High→5 | `clampRF(intent, число узлов)` = `max(1, min(intent, узлы))`, допускается 2, **pinned** |
| `Full` (SeaweedFS) | volume-серверы: `spec.storage.nodes`, иначе None→1 / Standard→3 / High→4 (master 1/3/3; filer 1, в High → 3) | код репликации `000`/`001`/`002` из `redundancy` |
| `Heavy` (Ceph RGW) | топология на стороне Ceph (`sds-elastic`) | `size` пула: None→2 / Standard→3 / High→4 |

**Фактор репликации фиксируется при инициализации (pinned).** Garage не
поддерживает смену `replication_factor` на живом кластере, поэтому контроллер
решает его один раз при создании (интент `redundancy`, ограниченный числом узлов
через `clampRF`; допускается rf=2), записывает в `garage.toml` и на последующих
reconcile **считывает обратно из работающего конфига**, а не пересчитывает по
текущему числу узлов. Хеш конфига проставляется в pod-template (аннотация
`storage.deckhouse.io/config-hash`), поэтому при изменении конфига поды
перекатываются и все ноды сходятся к одному фактору.

#### Модель хранения System (node-sticky local PV)

System держит **фиксированные 3 реплики** (`systemReplicas`) вне зависимости от
числа мастеров. Ключевая задача — чтобы каждая реплика при перезапуске/переезде
**возвращалась к своим данным**, иначе полный рестарт кластера теряет данные (см.
разбор ниже). Поэтому данные лежат не в «сыром» `hostPath`, а в **node-sticky local
PersistentVolume**:

- модуль создаёт StorageClass `sds-object-system-local`
  (`provisioner: kubernetes.io/no-provisioner`, `volumeBindingMode:
  WaitForFirstConsumer`, `reclaimPolicy: Retain`);
- контроллер выступает статическим провижинером: на каждом control-plane узле
  поддерживает пул из `systemReplicas` объектов `PersistentVolume` с источником
  `hostPath` (`type: DirectoryOrCreate` — каталог создаётся kubelet'ом при монтировании,
  агент на узле не нужен) и `spec.nodeAffinity`, прибитым к этому узлу (по
  `kubernetes.io/hostname`);
- StatefulSet использует `volumeClaimTemplates` на этом StorageClass — по PVC на
  ordinal (`data-<name>-garage-0/1/2`);
- `WaitForFirstConsumer` откладывает бинд PVC до планирования пода, поэтому
  планировщик выбирает узел **с учётом мягкого anti-affinity** (разнести по мастерам,
  когда их несколько; совместить на единственном, когда он один) и биндит PVC к
  свободному PV **на выбранном узле**.

Связка **PVC↔PV постоянна**, а `nodeAffinity` PV **жёстко возвращает под на его
узел** при любом перезапуске. Отсюда все свойства ниже.

Свежая установка на 1 мастере → 3 PV на нём → 3 пода совмещены (co-location).
Свежая установка на 3 мастерах → планировщик разносит поды по одному на мастер →
полная HA сразу. Кворум Garage привязан к числу подов (всегда 3), а не к числу
мастеров.

#### Сценарий: выключить все мастера и включить

**Диски сохранились (обычная перезагрузка).** PVC уже забинжены к своим PV,
`nodeAffinity` возвращает каждый под на его узел → под находит свои данные и `node_key`
(идентичность Garage тоже в `metadata_dir` на томе) → кластер поднимается с целыми
идентичностями, **без перемешивания и без потери**, дореплика не нужна.

Это главная причина отказа от «сырого» `hostPath` + `subPathExpr` по имени пода:
там имя пода не привязано к узлу, поэтому при одновременном рестарте планировщик мог
разложить поды **любой перестановкой**, каждый под вставал бы над пустым «своим»
подкаталогом на чужом узле, а старые данные оставались бы осиротевшими → тихая
потеря. Local PV убирает этот класс отказов.

**Диски стёрты (переустановка узлов).** Все три копии (rf=3) исчезают одновременно
→ потеря неизбежна: это свойство любого хранилища на локальных дисках, софтом не
лечится.

#### Сценарий: сжатие 3 → 2 → 1 и рост 1 → 2 → 3

Инвариант сохранности: **на каждом шаге для каждой партиции жива ≥1 копия в layout**.
При rf=3 и последовательных изменениях с ожиданием `healthy` между шагами это
соблюдается. Ключевой момент: `nodeAffinity` PV означает, что **данные сами не
переезжают** — их переносит только осознанный rebalance, оркестрируемый контроллером.

- **Сжатие 3 → 1** (вывод мастеров): у реплики на выведенном мастере PV становится
  непланируемым (`nodeAffinity` не удовлетворить) → под Pending. Данные этой реплики
  недоступны, но две другие копии живы → кворум (2) соблюдён, read-write. Чтобы
  вернуть реплику, контроллер пересоздаёт её PVC (см. rebalance) → она встаёт на
  оставшийся мастер и **дореплицируется** с живых. При 1 мастере все 3 реплики
  сходятся на нём (кворум собран, read-write).
- **Рост 1 → 2 → 3**: сами данные **не расползаются** — тома прибиты `nodeAffinity`
  к первому узлу. Но контроллер watch'ит Node, и как только мастеров становится ≥ 3,
  spread-реконсайл автоматически переносит co-located реплики на пустые мастера (по
  одной, с health-gate) → появляется аппаратная HA. 2 мастера — транзиент, игнорируется.

**Rolling rebalance (контроллер, по одной реплике):**

1. выбрать ordinal, чью реплику надо перенести (co-located, а есть свободный мастер);
2. пересоздать его PVC (удалить PVC + старый `Released` PV — `Retain`, данные не
   стираются); StatefulSet создаёт PVC заново;
3. `WaitForFirstConsumer` + anti-affinity → планировщик ставит под на новый мастер,
   PVC биндится к свободному PV там; под встаёт над пустым томом и **дореплицируется**
   с живых реплик (кворум 2 соблюдён);
4. дождаться `healthy` → повторить для следующей реплики.

**Операционный инвариант:** переносить/выводить по одной реплике, дожидаясь `healthy`
(полной дореплики) между шагами; никогда не выводить мастер, держащий последнюю
живую копию партиции.

**Реализация в контроллере (`reconcileSystemPlacement`, rebalance.go).** Контроллер
watch'ит control-plane Node (создание/удаление реконсилит System-ObjectStore —
`GenerationChangedPredicate` этого не видит) и на каждом reconcile делает не более
одного шага миграции:

- **мастеров ≥ 3 — spread:** реплику, co-located или прибитую к не-control-plane
  узлу, переносит на пустой мастер (soft anti-affinity сажает туда сам). Шаг **гейтится
  на `healthy`**: следующий перенос только после дореплики предыдущего (она
  завершается лишь при полном комплекте подов).
- **ровно 1 мастер — consolidate:** все реплики на него; переносятся те, что не на
  нём (Pending из-за удалённого мастера или на чужом узле). Без health-gate — у
  переносимых нет доступных данных (их узел ушёл), перенос лишь возвращает кворум;
  выжившую реплику-якорь никогда не recycle'им.
- **2 мастера — игнор** (транзиент сжатия: не растягиваем и не складываем).

Пул поддерживается `ensureSystemLocalPVs` (создаёт `systemReplicas` PV на каждый
control-plane узел; `gcSystemLocalPVs` убирает `Released` и `Available`-PV
удалённых мастеров). Особенности, учтённые из симуляции 3↔1: без Node-watch реакции
и корректного статуса не было; у survivor'а могло не быть свободных пул-PV (пере-создаём);
дореплика возможна только при 3/3 (`ensureMeshAndLayout` гейтится на готовности
StatefulSet), поэтому на consolidate поднимаем все поды на survivor'е, а не health-gate'им
по одной.

**Компромисс (приоритет — доступность).** Layout использует одну зону (`layoutZone`
= `dc1`), поэтому Garage разрешает совмещение реплик на одном хосте: на 1 мастере
(и в промежуточном состоянии на 2 хостах) избыточность — на уровне процессов, а не
железа. Настоящая аппаратная отказоустойчивость — только когда реплики разнесены на
3 разных мастера. Обратный выбор (зона на хост) дал бы честную избыточность, но
запретил бы 3 реплики на 1 мастере — сознательно не выбран.

**Стабильная идентичность ноды.** Ключевая проблема переезда: при recycle PVC
пересоздаётся пустым, поэтому наивно поднявшийся под сгенерировал бы **новый** node
ID, а старый остался бы в layout **мёртвой** нодой — Garage бесконечно ждёт слива
(drain) данных с неё, кластер виснет в `degraded`. На consolidate это доводило layout
до невосстановимого состояния (несколько мёртвых ID + запутанные версии layout →
`partitionsQuorum=0`, чинить нечем: зависшая нода живая, а не мёртвая, и
`skip-dead-nodes` не помогает). Поэтому идентичность реплики сохраняется отдельно от
тома данных:

- контроллер держит Secret `<resourceName>-node-identity` с ключами `node-<ord>` /
  `node-<ord>.pub` — по паре на ординал. `ensureNodeIdentities` на каждом reconcile
  снимает (`exec` + `base64`) `node_key`/`node_key.pub` с каждого Running-пода,
  идентичность которого ещё не сохранена (уже сохранённую **не перезаписывает** —
  она стабильна), с валидацией размеров (64/32 байта);
- Secret примонтирован в под опциональным volume'ом; init-контейнер `restore-node-key`
  до старта Garage восстанавливает `node_key` этого ординала из Secret'а в
  `metadata_dir`. Опциональность нужна для самого первого запуска: ключа ещё нет,
  Garage генерирует свой — контроллер его затем снимает.

Так recycle'нутая на другой мастер реплика (пустой том) поднимается со **своим**
прежним node ID, Garage дореплицирует данные через anti-entropy, а layout не
засоряется мёртвыми нодами. Идентичность снимается **до** любого шага ребаланса
(в `EnsureCluster` перед `reconcileSystemPlacement`), чтобы к моменту recycle она уже
была сохранена.

Мешинг реконсилит layout (`layoutRoleChanges`, mesh.go): при стабильной идентичности
вернувшаяся реплика приходит со **своим** node ID — обычный путь ничего не добавляет и
не удаляет, дореплика идёт через anti-entropy. Ветка добавления роли срабатывает лишь
для действительно нового ID, а удаление роли (только при полном комплекте живых реплик,
чтобы транзиент не вызывал лишних ребалансов) — защитный fallback для ID, который
по-настоящему не вернётся (например, идентичность потеряна до того, как её успели снять).

#### Особенности System

`System` — особый профиль (Garage, StatefulSet с фиксированным числом реплик на
control-plane узлах, данные в node-sticky local PV), поднимаемый модулем по умолчанию
вместе с системным бакетом и `BucketClaimPolicy` для `d8-*` namespace (отключается
параметром модуля). Правила CRD (CEL) фиксируют его отличия:

- имя ObjectStore обязано быть `system` (единственность + предсказуемые имена);
- `spec.redundancy` и `spec.storage.sizePerNode` задавать нельзя: ёмкость — это
  local PV на мастерах, фактор закреплён на 3 при init (не настраивается);
- `spec.storage.class` игнорируется: используется управляемый StorageClass
  `sds-object-system-local`;
- число реплик и фактор не зависят от числа мастеров (см. сценарии выше); поды
  перекатываются по `config-hash`, чтобы все ноды имели одинаковый
  `replication_factor`.

### 3.3 Status

```yaml
status:
  observedGeneration: 3
  phase: Ready                       # Pending | InProgress | Ready | Error
  backend:
    type: Garage                     # Garage | SeaweedFS | CephRGW
    version: "v1.0.1"
  endpoint:
    internal: http://system.d8-sds-object.svc.cluster.local
    region: us-east-1                # дефолтный регион S3
  capacity:
    total: 300Gi
    used: 42Gi
    available: 258Gi
    usedPercent: "14.00"
    lastUpdated: "2026-06-27T10:00:00Z"
  # Ссылка на Secret с админ-учёткой бэкенда (в namespace модуля)
  adminSecretRef:
    name: system-admin
  conditions:                        # patchMergeKey: type
    - type: BackendReady             # дата-плейн поднят и здоров
    - type: EndpointReady            # S3-эндпойнт доступен
    - type: Ready                    # агрегирующее
```

### 3.4 Валидация (webhook + x-kubernetes-validations)

- `metadata.name` — DNS-1123 label, `<= 30` символов.
- `spec.type` — immutable.
- `spec.elasticClusterRef` — обязателен и непуст ⇔ `type == Heavy`; immutable.
- `spec.storage.class` — обязателен для `Lightweight`/`Full`.
- `type == System`: размещение принудительно на control-plane, `storage.class`
  игнорируется (используется управляемый `sds-object-system-local`); валидатор
  предупреждает, если задан.
- Рекомендуется не более одного кластера `type: System` (имя по соглашению
  `system`), т. к. на него ссылаются платформенные модули.

### 3.5 Printer columns

```
Type        .spec.type
Phase       .status.phase
Endpoint    .status.endpoint.internal
Used%       .status.capacity.usedPercent      (priority 1)
Capacity    .status.capacity.total            (priority 1)
Ready       .status.conditions[?(@.type=="Ready")].status
Age         .metadata.creationTimestamp
```

## 4. CRD `Bucket` (cluster-scoped), `BucketAccess` (namespaced), `BucketClaimPolicy` (cluster-scoped)

### 4.1 `Bucket` — объявление бакета (без учётных данных)

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: Bucket
metadata:
  name: my-bucket              # cluster-scoped
spec:
  clusterRef: system           # имя ObjectStore — REQUIRED, immutable
  bucketName: ""               # имя бакета в S3; по умолчанию = metadata.name; immutable
  accessPolicy: Private        # Private | PublicRead   (default Private)
  reclaimPolicy: Retain        # Retain | Delete   (default Retain)
  quota:
    maxSize: 10Gi              # optional, лимит ёмкости бакета
    maxObjects: 0              # optional, 0 = без лимита
```

Status: `phase`, `endpoint`, `bucketName`, `conditions` (`BucketReady`, `Ready`).
Учётных данных бакет не содержит.

### 4.2 `BucketClaimPolicy` — кто допущен к бакету (deny-by-default)

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: BucketClaimPolicy
metadata:
  name: my-bucket-teams        # cluster-scoped
spec:
  bucketRef: my-bucket         # Bucket — REQUIRED, immutable
  allowedNamespaces:
    names: [my-app]            # точные имена namespace
    patterns: ["team-.*"]      # RE2-паттерны (полное совпадение)
```

Без совпадающей политики `BucketAccess` не провижнится (остаётся
`Pending` с condition `DeniedByPolicy`). Несколько политик на один бакет
складываются. Enforcement — в реконсайлере доступа (webhook лишь предупреждает).

### 4.3 `BucketAccess` — доступ и ключи (namespaced, self-service)

```yaml
apiVersion: storage.deckhouse.io/v1alpha1
kind: BucketAccess
metadata:
  name: my-access
  namespace: my-app
  annotations:
    storage.deckhouse.io/rotate: "1"   # смена значения → ротация ключей
spec:
  bucketRef: my-bucket         # cluster-scoped Bucket — REQUIRED, immutable
  permission: ReadWrite        # ReadWrite | ReadOnly   (default ReadWrite)
  secretName: ""               # optional; по умолчанию <access>-s3-credentials
```

Status: `phase`, `endpoint`, `bucketName`, `accessKeyID`, `secretRef`,
`observedRotation`, `lastRotationTime`, `conditions` (`AccessGranted`,
`CredentialsReady`, `Ready`).

### 4.4 Контракт Secret (стандартизированный для noops)

Контроллер создаёт в namespace доступа `Secret` (owned by
`BucketAccess`) с ключами, удобными для прямого `envFrom`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-access-s3-credentials
  namespace: my-app
type: Opaque
stringData:
  S3_ENDPOINT: http://system.d8-sds-object.svc.cluster.local
  S3_REGION: us-east-1
  S3_BUCKET: my-bucket
  AWS_ACCESS_KEY_ID: <generated per access>
  AWS_SECRET_ACCESS_KEY: <generated per access>
```

Каждый access получает **свою** пару ключей: независимый отзыв (удаление access)
и ротация (аннотация `storage.deckhouse.io/rotate` → новая пара, обновление
`Secret`, отзыв старого ключа).

### 4.5 Валидация

- `spec.clusterRef`/`spec.bucketRef` — immutable; ссылки должны существовать
  (иначе `Pending`, пока не появятся / не станут Ready).
- `spec.bucketName` (или `metadata.name`) — правила именования S3; immutable.
  Бакет уникален в рамках `(clusterRef, bucketName)`.
- `BucketClaimPolicy.spec.allowedNamespaces.patterns` — валидные RE2
  (webhook отклоняет некомпилируемые); доступ deny-by-default.

## 5. Архитектура контроллера

Один контроллер (controller-runtime), реконсайлеры на каждый CRD + слой
бэкенд-драйверов с общим интерфейсом:

```
ObjectStoreReconciler       ──┐
BucketReconciler         ─┼─→ backend.Driver (Garage | SeaweedFS | CephRGW)
BucketAccessReconciler   ─┤
BucketClaimPolicyReconciler   ──┘   (валидирует политику; enforcement — в Access-реконсайлере)
```

Интерфейс драйвера (бакет и доступ разделены):

```go
type Driver interface {
    EnsureCluster(ctx, *ObjectStore) (ClusterState, error)  // развернуть/обновить dataplane
    DeleteCluster(ctx, *ObjectStore) error                  // с учётом reclaimPolicy
    EnsureBucket(ctx, cluster, *Bucket) (BucketState, error)  // только бакет, без ключей
    DeleteBucket(ctx, cluster, *Bucket) error
    EnsureAccess(ctx, cluster, bucket, *BucketAccess, mintFresh bool) (AccessState, error)  // ключи per-access
    DeleteAccess(ctx, cluster, bucket, *BucketAccess) error
}
```

### 5.1 Реконсиляция кластера

- **System (Garage):** StatefulSet с фиксированным числом реплик (`systemReplicas` = 3),
  `nodeSelector: node-role.kubernetes.io/control-plane` и tolerations на мастера; мягкий
  (preferred) pod anti-affinity по `kubernetes.io/hostname`; `PodManagementPolicy: Parallel`.
  Данные — `volumeClaimTemplates` на управляемом StorageClass `sds-object-system-local`
  (node-sticky local PV, см. §3.2); контроллер держит пул статических `hostPath`-PV с
  `nodeAffinity` на control-plane узлах (`ensureSystemLocalPVs`) и убирает `Released` PV.
  Service (S3 API) и headless-сервис для членства; admin API для layout.
  `replication_factor` закреплён на 3 (независимо от числа мастеров); layout реконсилится
  при переезде/возврате реплик (см. §3.2). Админ-ключ → Secret в namespace модуля.
- **Lightweight (Garage):** StatefulSet с `volumeClaimTemplates` (`storage.class`,
  `storage.size`); число реплик = `replication_factor`, фактор достижим по построению.
- **Full (SeaweedFS):** master(ы) (raft 1/3), volume-servers (StatefulSet+PVC),
  filer со встроенным S3-gateway; Service на S3-эндпойнт; репликация/EC из `redundancy`.
  Метаданные filer: встроенный leveldb (None/Standard, один filer, без внешних
  зависимостей) или общий PostgreSQL из `managed-postgres` (High, multi-filer HA).
- **Heavy (Ceph RGW):** контроллер создаёт `CephObjectStore` в namespace
  `sds-elastic` (`d8-sds-elastic`), привязанный к CephCluster из `elasticClusterRef`;
  RGW-поды и Service поднимает Rook; пулы метаданных/данных — из `redundancy`.
  `preservePoolsOnDelete` завязан на `reclaimPolicy` кластера (Retain → пулы
  сохраняются).

### 5.2 Реконсиляция бакета и доступа

Бакет (`Bucket`):

1. Найти `ObjectStore` по `clusterRef`, дождаться `Ready`.
2. Через admin/S3 API бэкенда создать бакет (если нет), применить policy/quota.
3. Финализатор: при удалении — удалить бакет только при `reclaimPolicy: Delete`.

Доступ (`BucketAccess`):

1. Найти бакет по `bucketRef`, дождаться `Ready`; проверить, что namespace
   разрешён `BucketClaimPolicy` (deny-by-default). При отзыве политики у
   уже выданного доступа ключ **отзывается**, `Secret` удаляется.
2. Выдать отдельную пару ключей (Garage key / SeaweedFS identity / Ceph RGW user
   + bucket policy) с правами из `permission`; при смене аннотации ротации —
   новая пара + отзыв старого ключа.
3. Записать `Secret` в namespace доступа (§4.4), проставить `status.secretRef`.
4. Финализатор: при удалении `BucketAccess` — отозвать ключ.

Системное хранилище: модуль по умолчанию (`sdsObject.systemBucket.enabled`,
default true) шипует системный `ObjectStore` (`System`) + `system`-бакет
+ политику на `d8-*`; `redundancy` кластера следует режиму HA
(`helm_lib_is_ha_to_value`: HA → Standard, иначе None).

Маппинг admin-операций по бэкендам: Garage Admin API; SeaweedFS S3/filer API;
Ceph RGW admin ops (или `CephObjectStoreUser` + bucket через S3 от его имени).

## 6. Настройки модуля (openapi/config-values.yaml)

Только политики оператора, никаких per-cluster ручек (как у sds-elastic):

```yaml
logLevel: INFO
controller:
  resourcesManagement: { mode: VPA, ... }    # ресурсы контроллера
nodeSelector: {}                              # размещение служебных подов модуля
tolerations: []
systemBucket:
  enabled: true                               # шипать системный кластер+бакет (default true)
```

Вся специфика кластеров и бакетов — только через CRD.

## 7. Зависимости

- **Heavy** требует установленного и настроенного `sds-elastic` (Rook Ceph) с
  готовым `ElasticCluster`. Жёсткую зависимость в `module.yaml` ставить не нужно
  — она нужна только для типа Heavy; проверяется в рантайме реконсайлером
  (условие `Error`/`Pending`, если sds-elastic недоступен).
- System/Lightweight/Full самодостаточны (Garage/SeaweedFS вендорятся в образы
  модуля). Исключение: `Full` в режиме `High` требует модуля
  `managed-postgres` (общий слой метаданных filer); в остальных режимах — нет.

## 8. Что меняется относительно текущего скелета

> **Неактуально (исторический раздел).** Скелет давно заменён рабочей
> реализацией: оба CRD, три драйвера, оба реконсайлера, вебхуки и хуки готовы,
> а модель бакетов переработана (см. врезку в начале документа и `crds/`).
> Раздел оставлен для истории миграции с placeholder-CRD `ObjectStorageClass`.

1. `crds/objectstorageclass.yaml` → удалить; добавить
   `crds/objectstoragecluster.yaml` и `crds/objectbucket.yaml`.
2. `api/v1alpha1/object_storage_class.go` → заменить на
   `object_storage_cluster.go` и `object_bucket.go` (+ enum-константы типов,
   redundancy, фаз, conditions).
3. Хуки `030-remove-finalizers-on-module-delete` и `consts/consts.go`:
   обновить `CRGVKsForFinalizerRemoval` (две GVK; `ObjectBucket` namespaced=true),
   `WebhookConfigurationsToDelete`, `AllowedProvisioners`.
4. Вебхуки: заменить `/osc-validate` на валидаторы
   `/objectstoragecluster-validate` (cluster) и `/objectbucket-validate`
   (namespaced); правила immutable/обязательности из §3.4 и §4.4.
5. RBAC (`templates/rbacv2/manage/`): edit/view на оба новых CRD; учесть, что
   `ObjectBucket` namespaced (нужны namespaced-роли для self-service команд).
6. Образы werf: добавить образы дата-плейна (garage, seaweedfs) и/или их
   вендоринг; для Heavy — только интеграция с Rook, отдельный образ не нужен.

## 9. Открытые вопросы

- Reclaim-политика (удалять данные или сохранять) — **реализована**:
  `reclaimPolicy: Retain|Delete` (default `Retain`) на бакете и на кластере
  (для Heavy `Retain` сохраняет пулы Ceph). Вопрос закрыт.
- Owner-тег бэкенд-бакета: `EnsureBucket` пока переиспользует уже существующий
  бакет по имени без проверки владельца — остаточный вопрос изоляции (см. ADR).
- Мульти-регион / геораспределение Garage — вне первой версии.
- Экспонирование S3-эндпойнта наружу кластера (Ingress) — вне первой версии,
  пока только внутрикластерный Service.
- Нужен ли отдельный системный кластер по умолчанию (авто-провижн) — решено:
  только через явный CR `type: System`.
