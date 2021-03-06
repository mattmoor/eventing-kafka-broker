/*
 * Copyright 2020 The Knative Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package dev.knative.eventing.kafka.broker.receiver;

import static dev.knative.eventing.kafka.broker.core.testing.utils.CoreObjects.broker1;
import static org.junit.jupiter.api.Assertions.fail;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import io.vertx.core.AsyncResult;
import io.vertx.core.Future;
import io.vertx.core.Handler;
import io.vertx.core.http.HttpServerRequest;
import io.vertx.core.http.HttpServerResponse;
import io.vertx.kafka.client.producer.KafkaProducer;
import io.vertx.kafka.client.producer.RecordMetadata;
import io.vertx.kafka.client.producer.impl.KafkaProducerRecordImpl;
import java.util.HashSet;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.Test;

public class RequestHandlerTest {

  private static final int TIMEOUT = 3;

  @Test
  public void shouldSendRecordAndTerminateRequestWithRecordProduced() throws InterruptedException {
    shouldSendRecord(false, RequestHandler.RECORD_PRODUCED);
  }

  @Test
  public void shouldSendRecordAndTerminateRequestWithFailedToProduce() throws InterruptedException {
    shouldSendRecord(true, RequestHandler.FAILED_TO_PRODUCE);
  }

  @SuppressWarnings("unchecked")
  private static void shouldSendRecord(boolean failedToSend, int statusCode)
      throws InterruptedException {
    final var record = new KafkaProducerRecordImpl<>(
        "topic", "key", "value", 10
    );

    final RequestToRecordMapper<String, String> mapper
        = request -> Future.succeededFuture(record);

    final KafkaProducer<String, String> producer = mock(KafkaProducer.class);

    when(producer.send(any(), any())).thenAnswer(invocationOnMock -> {

      final var handler = (Handler<AsyncResult<RecordMetadata>>) invocationOnMock
          .getArgument(1, Handler.class);
      final var result = mock(AsyncResult.class);
      when(result.failed()).thenReturn(failedToSend);
      when(result.succeeded()).thenReturn(!failedToSend);

      handler.handle(result);

      return producer;
    });

    final var broker = broker1();

    final var request = mock(HttpServerRequest.class);
    when(request.path()).thenReturn(String.format("/%s/%s", broker.namespace(), broker.name()));
    final var response = mockResponse(request, statusCode);

    final var handler = new RequestHandler<>(producer, mapper);

    final var countDown = new CountDownLatch(1);

    handler.reconcile(Map.of(broker, new HashSet<>()))
        .onFailure(cause -> fail())
        .onSuccess(v -> countDown.countDown());

    countDown.await(TIMEOUT, TimeUnit.SECONDS);

    handler.handle(request);

    verifySetStatusCodeAndTerminateResponse(statusCode, response);
  }

  @Test
  @SuppressWarnings({"unchecked"})
  public void shouldReturnBadRequestIfNoRecordCanBeCreated() throws InterruptedException {
    final var producer = mock(KafkaProducer.class);

    final RequestToRecordMapper<Object, Object> mapper
        = (request) -> Future.failedFuture("");

    final var broker = broker1();

    final var request = mock(HttpServerRequest.class);
    when(request.path()).thenReturn(String.format("/%s/%s", broker.namespace(), broker.name()));
    final var response = mockResponse(request, RequestHandler.MAPPER_FAILED);

    final var handler = new RequestHandler<Object, Object>(producer, mapper);

    final var countDown = new CountDownLatch(1);
    handler.reconcile(Map.of(broker, new HashSet<>()))
        .onFailure(cause -> fail())
        .onSuccess(v -> countDown.countDown());

    countDown.await(TIMEOUT, TimeUnit.SECONDS);

    handler.handle(request);

    verifySetStatusCodeAndTerminateResponse(RequestHandler.MAPPER_FAILED, response);
  }

  private static void verifySetStatusCodeAndTerminateResponse(
      final int statusCode,
      final HttpServerResponse response) {
    verify(response, times(1)).setStatusCode(statusCode);
    verify(response, times(1)).end();
  }

  private static HttpServerResponse mockResponse(
      final HttpServerRequest request,
      final int statusCode) {

    final var response = mock(HttpServerResponse.class);
    when(response.setStatusCode(statusCode)).thenReturn(response);

    when(request.response()).thenReturn(response);
    return response;
  }
}