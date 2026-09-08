#include <libyuv/convert.h>
#include <libyuv/convert_argb.h>
#include <libyuv/convert_from.h>
#include <libyuv/convert_from_argb.h>
#include <libyuv/rotate.h>
#include <libyuv/rotate_argb.h>

// BGRA(内存序) 等比缩放（libyuv ARGBScale；filter: 0=none 1=linear 2=bilinear 3=box）
int ARGBScale(const uint8_t* src_argb, int src_stride_argb, int src_width, int src_height,
              uint8_t* dst_argb, int dst_stride_argb, int dst_width, int dst_height,
              int32_t filtration);
