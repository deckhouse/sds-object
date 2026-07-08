---
title: "Модуль sds-object"
description: "Модуль Deckhouse Kubernetes Platform для управления S3-совместимым объектным хранилищем."
weight: 1
---

{{< alert level="warning" >}}
Модуль находится в стадии `Experimental`. API, конфигурация и пользовательские ресурсы могут меняться без предупреждения; не используйте его для production-нагрузок.
{{< /alert >}}

Модуль `sds-object` управляет S3-совместимым объектным хранилищем в кластере Deckhouse Kubernetes Platform. Подход no-ops: вы декларируете *что* вам нужно набором COSI-совместимых пользовательских ресурсов, а модуль сам разворачивает и сопровождает бэкенд. Низкоуровневые настройки бэкенда наружу не выносятся.

## Пользовательские ресурсы

| Ресурс | Область | Назначение |
|--------|---------|------------|
| `ObjectStore` (`ostore`) | Cluster | Разворачивает объектное хранилище одного из четырёх готовых профилей (разворачивает data plane; вне COSI). |
| `Bucket` (`bkt`) | Cluster | Бакет-бэкенд: либо объявлен администратором (Shared), либо создан под `BucketClaim`. |
| `BucketClaim` (`bc`) | Namespaced | Запрос («заявка») на бакет: greenfield (создаёт собственный приватный бакет) или brownfield (привязывает Shared-бакет, гейтится `BucketPolicy`). |
| `BucketAccess` (`ba`) | Namespaced | Запрашивает учётные данные к `BucketClaim`; пишет стандартный `Secret` с S3-учётками рядом с приложением. |
| `BucketPolicy` (`bp`) | Cluster | Задаёт, из каких namespace разрешена привязка Shared-бакета через brownfield-`BucketClaim` (deny-by-default). |

Модель повторяет COSI (`Bucket` + `BucketClaim`) с двумя дополнениями: `ObjectStore` разворачивает само хранилище, а `BucketPolicy` управляет совместным доступом (sharing) к Shared-бакетам между namespace. Учётные данные выдаются по namespace через `BucketAccess`, ссылающийся на `BucketClaim` в том же namespace; каждый access получает свой ключ, независимо ротируемый аннотацией `storage.deckhouse.io/rotate`.

## Профили кластера

Поле `spec.type` выбирает профиль; бэкенд подбирается и сопровождается за вас.

| `spec.type` | Бэкенд | Размещение / данные | Сценарий |
|-------------|--------|---------------------|----------|
| `System` | Garage | DaemonSet на control-plane узлах, `hostPath` | Системные нужды платформы (бэкапы, registry, логи). |
| `Lightweight` | Garage | StatefulSet, PVC на StorageClass | Небольшие прикладные нагрузки. |
| `Full` | SeaweedFS | StatefulSet, PVC | Масштабируемое полнофункциональное хранилище. |
| `Heavy` | Ceph RGW | RADOS Gateway поверх существующего кластера sds-elastic | Переиспользует ёмкость и отказоустойчивость Ceph. |

{{< alert level="info" >}}
Статус реализации: доступны все четыре профиля. `Full` запускает распределённый SeaweedFS (master/volume/filer + S3-gateway; число реплик и репликация масштабируются от `redundancy`). Метаданные filer хранятся в общем PostgreSQL, разворачиваемом модулем [managed-postgres](../../managed-postgres/stable/), благодаря чему filer/S3-gateway работает в нескольких репликах (HA). `Heavy` разворачивает Ceph RGW поверх существующего кластера sds-elastic. Бакеты работают на всех четырёх профилях.

Поэтому профиль `Full` требует включённого модуля `managed-postgres`.
{{< /alert >}}

## Как это работает

- Контроллер реконсилит каждый `ObjectStore` в data plane бэкенда (workload'ы, сервисы, конфигурация) и публикует в `status` готовность, S3-эндпойнт и ёмкость.
- Контроллер реконсилит каждый `Bucket` в бакет в указанном ObjectStore (без учётных данных).
- Контроллер реконсилит каждый `BucketClaim`: greenfield-заявка создаёт собственный приватный `Bucket`; brownfield-заявка привязывает существующий Shared-`Bucket`, как только `BucketPolicy` разрешает её namespace.
- Контроллер реконсилит каждый `BucketAccess` — как только его `BucketClaim` в состоянии Bound — в ключ доступа с правами на бакет, затем пишет `Secret` (owned by access) со стандартными переменными подключения: `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`.

Пошаговый пример — в разделе [Использование](usage.html).

## Требования

- Профиль `Heavy` требует модуль [`sds-elastic`](/modules/sds-elastic/) с готовым `ElasticCluster`; он используется только для `Heavy`-кластеров.
- Профили `System`, `Lightweight` и `Full` самодостаточны (образы бэкендов поставляются вместе с модулем).
