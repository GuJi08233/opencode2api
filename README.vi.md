# opencode2api

[English](README.md) | [简体中文](README.zh-CN.md) | [Tiếng Việt](README.vi.md)

Gateway Go dành cho **các mô hình miễn phí, ẩn danh của OpenCode**, dựa trên nhánh [free-only của upstream](https://github.com/jasonxu114514/opencode2api/tree/free-only). Cung cấp API Chat Completions, Responses, Anthropic Messages và System One. Fork này tiếp tục hỗ trợ Docker/GHCR và các tùy chọn thử lại cho proxy luân phiên.

Mọi yêu cầu suy luận lên upstream đều dùng thông tin xác thực `public` của OpenCode; không cần API Key Zen hoặc Go. Pool khóa trả phí, WebUI, API quản trị và nhóm luân phiên `sota` / `sweet` đã bị loại bỏ. Cấu hình được đọc khi khởi động; cần khởi động lại sau khi sửa.

## Bắt đầu nhanh

Biên dịch bằng **Go 1.24 trở lên**:

```bash
git clone https://github.com/GuJi08233/opencode2api.git
cd opencode2api
cp config.example.json config.json
# Thay server_keys bằng API Key cục bộ của bạn.
go build -o opencode2api ./cmd/opencode2api
./opencode2api -config config.json
```

API mặc định lắng nghe tại `127.0.0.1:8080`. `-config` chọn tệp cấu hình (mặc định `config.json`); `-listen` ghi đè địa chỉ API. Không còn dịch vụ quản trị hay cổng 8081.

## Docker

Image của fork là `ghcr.io/guji08233/opencode2api`, hỗ trợ `linux/amd64` và `linux/arm64`. Dùng thẻ phát hành để cố định phiên bản; `latest` trỏ đến bản ổn định được phát hành gần nhất.

```bash
cp config.example.json config.json
# Thay server_keys và cấu hình proxy nếu cần.
docker compose up -d
docker compose logs -f
```

Compose chỉ công bố cổng **8080**, lưu cấu hình và bộ nhớ đệm trong volume `opencode2api-state`. Tệp `config.json` trên máy chủ **chỉ được nhập khi khởi động lần đầu**. Để áp dụng những lần sửa sau, đặt `listen` trong tệp máy chủ thành `0.0.0.0:8080`, sao chép tệp vào container đang chạy rồi khởi động lại:

```bash
docker compose cp config.json opencode2api:/var/lib/opencode2api/config.json
docker compose restart
```

| Biến Compose | Mặc định | Công dụng |
| --- | --- | --- |
| `OPENCODE2API_VERSION` | `latest` | Thẻ image. |
| `OPENCODE2API_PORT` | `8080` | Cổng máy chủ ánh xạ tới cổng 8080 của container. |
| `OPENCODE2API_LISTEN` | Không đặt | Ghi đè địa chỉ API trong container. |

Khi nhập lần đầu, entrypoint đổi địa chỉ mẫu `127.0.0.1:8080` thành `0.0.0.0:8080`. Đổi cổng máy chủ không đổi địa chỉ lắng nghe trong container. Nếu dùng `proxyfile`, hãy mount tệp đó vào đúng đường dẫn cấu hình trong container. Container chạy bằng người dùng không đặc quyền, hệ thống tệp gốc chỉ đọc và volume trạng thái có thể ghi.

Biên dịch và chạy image cục bộ:

```bash
docker build -t opencode2api:local .
docker run -d --name opencode2api \
  -p 8080:8080 \
  -e CONFIG_SEED_PATH=/run/config/opencode2api.json \
  -v "$(pwd)/config.json:/run/config/opencode2api.json:ro" \
  -v opencode2api-state:/var/lib/opencode2api \
  opencode2api:local
```

## Sử dụng API

Xác thực bằng `Authorization: Bearer YOUR_LOCAL_API_KEY` hoặc `x-api-key: YOUR_LOCAL_API_KEY`, khớp với khóa đang bật trong `server_keys`. Các khóa này bảo vệ gateway, không dùng để xác thực với upstream. `/healthz` không yêu cầu xác thực.

| Phương thức | Đường dẫn | Công dụng |
| --- | --- | --- |
| GET | `/v1/models` | Mô hình miễn phí hiện có thể định tuyến và giao thức gốc. |
| POST | `/v1/chat/completions` | Chat Completions. |
| POST | `/v1/responses` | Responses. |
| POST | `/v1/messages` | Anthropic Messages. |
| POST | `/v1/systemone` | Quyết định có cấu trúc theo giao thức System One gốc. |
| GET | `/healthz` | Trạng thái sẵn sàng, danh mục mô hình và số proxy. |

Lấy danh sách mô hình trước, rồi thay `MODEL_ID` bằng ID trong kết quả:

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY"

curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","messages":[{"role":"user","content":"Xin chào"}]}'

curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","input":"Xin chào"}'

curl http://localhost:8080/v1/messages \
  -H "x-api-key: YOUR_LOCAL_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","max_tokens":512,"messages":[{"role":"user","content":"Xin chào"}]}'
```

Chat, Responses và Messages hỗ trợ JSON và SSE; dùng `curl -N` và thêm `"stream":true` để nhận luồng. Văn bản, hình ảnh, suy luận và công cụ được chuyển đổi khi giao thức đích hỗ trợ. Yêu cầu theo giao thức gốc giữ các trường riêng của nhà cung cấp; việc chuyển đổi giữa giao thức không giữ được mọi tùy chọn. Giới hạn nội dung yêu cầu là 32 MiB. Phản hồi có `x-request-id`.

System One dùng cấu trúc riêng gồm `state` / `questions`, yêu cầu mô hình có `native_protocol: "systemone"`. Nội dung được chuyển tiếp nguyên trạng, không chuyển đổi qua giao thức chat:

```bash
curl http://localhost:8080/v1/systemone \
  -H "Authorization: Bearer YOUR_LOCAL_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"MODEL_ID","state":"1 + 1 = 2","questions":{"arithmetic":{"type":"noul","instructions":"Is 1 + 1 equal to 2?"}}}'
```

## Định tuyến mô hình miễn phí

Gateway thử đường dẫn cũ `https://opencode.ai/zen/v1/*` trước, sau đó chuyển sang `https://opencode.ai/inference/v1/*` khi cần. Cả hai đều dùng truy cập ẩn danh.

Mô hình vượt qua kiểm tra miễn phí nếu ID chứa `free` (không phân biệt hoa thường), **hoặc** metadata OpenCode ghi cả giá đầu vào và đầu ra bằng 0 và mô hình chưa bị ngừng hỗ trợ. Khi metadata chưa có hoặc thiếu mô hình, chỉ áp dụng quy tắc tên. Danh mục upstream, giao thức gốc và kiểm tra khả dụng tiếp tục giới hạn kết quả `/v1/models`. ID chứa `free` vẫn vượt qua kiểm tra giá ngay cả khi metadata đánh dấu ngừng hỗ trợ, nhưng vẫn phải thỏa các điều kiện định tuyến khác.

Danh mục được làm mới theo `models.refresh_seconds` (mặc định 300 giây). Metadata giá và trạng thái ngừng hỗ trợ từ `https://models.opencode.ai/api.json` được làm mới mỗi 24 giờ. Khi cập nhật thất bại, dữ liệu đệm được giữ lại. `models.protocols` ghi đè giao thức gốc bằng `chat`, `responses`, `anthropic` hoặc `systemone`.

`/healthz` trả 503 khi danh mục chưa sẵn sàng, không có mô hình định tuyến được hoặc không có proxy khỏe mạnh. Danh mục đệm cũ nhưng còn dùng được trả 200 kèm trạng thái suy giảm. Kiểm tra này không đảm bảo yêu cầu suy luận tiếp theo sẽ thành công.

## Cấu hình và proxy

Xem [config.example.json](config.example.json). JSON hỗ trợ chú thích. Trường không nhận diện sẽ bị bỏ qua, kể cả các trường cũ `zen_keys`, `go_keys`, `anonymous`, `prefer`, `webui`, `rotation` và `models.free_only`. Chế độ chỉ miễn phí luôn bật. `server_keys`, `proxies` và tùy chọn thử lại riêng của fork vẫn được hỗ trợ. Cần xóa cờ đã bị loại bỏ như `-web-listen` khỏi lệnh khởi động dịch vụ.

| Thiết lập | Mặc định / ý nghĩa |
| --- | --- |
| `listen` | `127.0.0.1:8080`. |
| `server_keys` | Phải có ít nhất một khóa cục bộ được bật. |
| `upstream.zen` | `https://opencode.ai/zen`, đường dẫn ẩn danh cũ. |
| `upstream.anonymous` | `https://opencode.ai/inference`, đường dẫn dự phòng. |
| `proxies` / `proxyfile` | Gộp và loại trùng; dùng `direct` nếu cả hai trống. Đường dẫn tương đối tính từ thư mục cấu hình. |
| `retry.max_attempts` | `3`; kiểm soát số lượt truy cập proxy khi có pool luân phiên. |
| `retry.anonymous_max_attempts` | `0`; giá trị dương giới hạn tổng số yêu cầu HTTP ẩn danh thực tế qua cả hai đường dẫn, yêu cầu `disable_proxy_keep_alives: true`. |
| `retry.timeout_seconds` | `300`; thời gian chờ tổng cho suy luận, gồm cả luồng. |
| `performance.disable_proxy_keep_alives` | `false`; kết nối mới cho yêu cầu qua proxy, tắt HTTP/2. Kết nối trực tiếp vẫn được tái sử dụng. |
| `performance.attempt_timeout_seconds` | `0`; dùng thời gian chờ tổng. Giá trị dương giới hạn từng lần thử upstream. |
| `performance.first_event_timeout_seconds` | `0`; tắt thời gian chờ sự kiện đầu tiên. |
| `performance.connect_timeout_seconds` | `5`. |
| `performance.failure_cooldown_seconds` | `15`; thời gian nghỉ cơ sở khi proxy tĩnh lỗi. |
| `performance.max_idle_conns` / `max_idle_conns_per_host` | `4096` / `1024` cho mỗi transport. |
| `performance.max_conns_per_host` | `0` (không giới hạn). |
| `performance.idle_conn_timeout_seconds` | `120`. |
| `reasoning.effort` / `effort_by_model` | Mức suy luận mặc định và theo mô hình; mức do client chỉ định rõ được ưu tiên. |
| `logging.level` / `dump_request_bodies` | `info` / `false`; ghi nội dung yêu cầu còn cần mức debug. |

Proxy hỗ trợ `direct`, `http://`, `https://`, `socks5://`, `socks5h://`, gồm thông tin đăng nhập trong URL. Tệp proxy có một mục mỗi dòng, hỗ trợ dòng trống và chú thích `#`, `;`, `//` ở đầu dòng hoặc sau khoảng trắng.

### Gateway proxy luân phiên

Gộp các trường sau vào cấu hình, dùng thông tin đăng nhập của bạn:

```json
{
  "proxies": ["socks5h://USER:PASSWORD@resin:2260"],
  "retry": {"max_attempts": 3, "anonymous_max_attempts": 3, "timeout_seconds": 300},
  "performance": {"disable_proxy_keep_alives": true, "attempt_timeout_seconds": 20}
}
```

Giá trị dương của `anonymous_max_attempts` đếm **yêu cầu HTTP thực tế**, gồm lần đầu, chuyển đổi giữa hai đường dẫn upstream và lần thử lại do tham chiếu suy luận hết hiệu lực. Tất cả dùng chung thời gian chờ tổng. Chế độ này yêu cầu thiết lập toàn cục `performance.disable_proxy_keep_alives: true`; đánh dấu riêng `#pool` không thay thế được yêu cầu đó. Proxy luân phiên có thể được truy cập lại mà một IP lỗi không làm toàn bộ đầu vào phải nghỉ; mục trực tiếp chỉ được ghé một lượt trong mỗi vòng định tuyến. Sau khi đã gửi dữ liệu luồng cho client, yêu cầu không được phát lại.

Kết nối mới cho phép gateway proxy chọn IP thoát khác. Việc đổi IP và giữ phiên do gateway proxy quyết định.

Có thể đánh dấu từng pool bằng hậu tố `#pool` hoặc tiền tố `pool+`:

```text
http://127.0.0.1:7890#pool
pool+socks5h://127.0.0.1:1080
```

Các mục này luôn dùng kết nối mới, bỏ qua việc loại proxy theo sức khỏe và thời gian nghỉ do lỗi, đồng thời có thể được truy cập lại. Khi `anonymous_max_attempts: 0`, proxy tĩnh chỉ được ghé một lượt; nếu có pool, ngân sách lượt truy cập ít nhất là `retry.max_attempts`. Mỗi lượt có thể thử cả hai đường dẫn upstream, vì vậy số lượt mặc định không phải số yêu cầu HTTP. Proxy tĩnh giữ cơ chế tái sử dụng kết nối nếu tùy chọn kết nối mới toàn cục chưa bật.

## Lưu trữ và phát triển

Thư mục cấu hình cần quyền ghi để lưu bộ nhớ đệm danh mục, giá và khả dụng. Nhật ký dịch vụ dùng JSON trên stdout; lỗi trước khi khởi tạo bộ ghi nhật ký xuất ra stderr. Số liệu trong bộ nhớ được xóa khi khởi động lại. Không có trình sửa cấu hình hay API nạp lại.

Nhánh free-only của upstream tập trung tối ưu xử lý đồng thời yêu cầu ẩn danh và tăng pool kết nối. Thông lượng thực tế phụ thuộc giới hạn upstream, proxy và khối lượng công việc.

```bash
go test ./...
go vet ./...
go build ./cmd/opencode2api
```

[Giấy phép GPL-3.0-or-later](LICENSE).
