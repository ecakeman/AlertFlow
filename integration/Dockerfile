FROM eclipse-temurin:25-jdk
WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends maven && rm -rf /var/lib/apt/lists/*
COPY proto /src/proto
COPY integration /src/integration
WORKDIR /src/integration
RUN mkdir -p /root/.m2 && cp maven-settings.xml /root/.m2/settings.xml && mvn -B -DskipTests package
EXPOSE 8081 9091
HEALTHCHECK --interval=5s --timeout=3s --retries=20 CMD bash -c 'echo > /dev/tcp/127.0.0.1/8081'
ENTRYPOINT ["java", "-jar", "target/integration-0.1.0.jar"]
