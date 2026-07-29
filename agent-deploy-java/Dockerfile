# Build stage using Maven and JDK 17
FROM maven:3.9-eclipse-temurin-17 AS builder
WORKDIR /workspace

# Copy pom.xml and source code
COPY pom.xml .
COPY src ./src

# Build executable JAR package
RUN mvn clean package -DskipTests

# Runtime stage using JRE 17
FROM eclipse-temurin:17-jre-alpine
WORKDIR /app

# Copy packaged JAR from builder stage
COPY --from=builder /workspace/target/agent-deploy-java-1.0.0-SNAPSHOT.jar app.jar

# Run as Operator (Controller)
ENTRYPOINT ["java", "-jar", "app.jar", "--OPERATOR_ENABLED=true", "--APISERVER_ENABLED=false"]
