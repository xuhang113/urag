# 生产索引用 smartcn。基于官方 Elasticsearch 8.17.4，入口保持镜像原样。
FROM docker.elastic.co/elasticsearch/elasticsearch:8.17.4
RUN elasticsearch-plugin install --batch analysis-smartcn
